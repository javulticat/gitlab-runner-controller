package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

func TestDiscoveryPaginationAndMissingJobConfirmation(t *testing.T) {
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "secret" {
			t.Error("missing API authentication")
		}
		calls = append(calls, r.URL.RequestURI())
		if strings.HasSuffix(r.URL.Path, "/jobs/900") {
			fmt.Fprint(w, `{"id":900,"status":"success","tag_list":["linux"],"ref":"v1","tag":true,"runner":{"id":42},"runner_manager":{"id":99},"pipeline":{"id":100}}`)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			if len(r.URL.Query()["scope[]"]) != 4 {
				t.Error("active statuses are incomplete")
			}
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"id":1,"status":"pending","tag_list":["linux"]}]`)
		} else {
			fmt.Fprint(w, `[{"id":2,"status":"running","tag_list":["linux"],"ref":"main","tag":false,"runner":{"id":43},"pipeline":{"id":101}}]`)
		}
	}))
	defer server.Close()
	jobs, err := New(server.URL+"/gitlab", "secret").Sync(context.Background(), []int64{1}, []state.Job{{ProjectID: 1, ID: 900}, {ProjectID: 1, ID: 900}})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 || len(calls) != 3 || !strings.HasPrefix(calls[0], "/gitlab/api/v4/projects/1/jobs?") {
		t.Fatalf("incomplete synchronization: %v %v", jobs, calls)
	}
	for _, job := range jobs {
		switch job.ID {
		case 900:
			if job.RunnerID != 42 {
				t.Fatalf("confirmation lost job attribution: %+v", job)
			}
		case 2:
			if job.RunnerID != 43 {
				t.Fatalf("discovery lost job attribution: %+v", job)
			}
		}
	}
}

func TestInvalidAndIncompleteSnapshotsFailClosed(t *testing.T) {
	for _, test := range []struct {
		name, body, next string
		status           int
	}{
		{"null", "null", "", 200}, {"malformed", "{", "", 200}, {"invalid page", "[]", "9", 200},
		{"new status", `[{"id":1,"status":"new_future_status"}]`, "", 200},
		{"missing ID", `[{"status":"running"}]`, "", 200}, {"permissions", "secret-data", "", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Next-Page", test.next)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			_, err := New(server.URL, "secret").Sync(context.Background(), []int64{1}, nil)
			if err == nil || strings.Contains(err.Error(), "secret-data") {
				t.Fatalf("unsafe synchronization result: %v", err)
			}
		})
	}
}

func TestRateLimitAndDeletedConfirmation(t *testing.T) {
	limited := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limited {
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(429)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/99") {
			w.WriteHeader(404)
			return
		}
		if err := json.NewEncoder(w).Encode([]apiJob{}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client := New(server.URL, "secret")
	_, err := client.Sync(context.Background(), []int64{1}, nil)
	var limit *RateLimitError
	if !errors.As(err, &limit) || limit.Delay != 90*time.Second {
		t.Fatalf("rate limit lost: %v", err)
	}
	limited = false
	jobs, err := client.Sync(context.Background(), []int64{1}, []state.Job{{ProjectID: 1, ID: 99}})
	if err != nil || len(jobs) != 0 {
		t.Fatalf("verified deleted job was retained: %v %v", jobs, err)
	}
}

func TestDeletedJobHintDoesNotBlockNewDemand(t *testing.T) {
	newDemand := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/1/jobs/99":
			w.WriteHeader(http.StatusNotFound)
		case "/api/v4/projects/2/jobs":
			fmt.Fprint(w, `[{"id":200,"status":"pending","tag_list":["linux"]}]`)
		case "/api/v4/projects/1/jobs":
			if newDemand {
				fmt.Fprint(w, `[{"id":100,"status":"pending","tag_list":["linux"]}]`)
			} else {
				fmt.Fprint(w, `[]`)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client := New(server.URL, "secret")
	jobs, err := client.Sync(context.Background(), []int64{1, 2}, []state.Job{{ProjectID: 1, ID: 99}})
	if err != nil || len(jobs) != 1 || jobs[0].ID != 200 {
		t.Fatalf("deleted hint blocked other project demand: %v %v", jobs, err)
	}
	newDemand = true
	jobs, err = client.Sync(context.Background(), []int64{1}, nil)
	if err != nil || len(jobs) != 1 || jobs[0].ID != 100 {
		t.Fatalf("deleted hint blocked later demand: %v %v", jobs, err)
	}
}

func TestFailedActiveJobDiscoveryCannotConfirmDeletion(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing project", http.StatusNotFound, "secret-data"},
		{"unauthorized", http.StatusUnauthorized, "secret-data"},
		{"forbidden", http.StatusForbidden, "secret-data"},
		{"invalid later page", http.StatusOK, `[{"id":1,"status":"future_status"}]`},
		{"malformed later page", http.StatusOK, `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			confirmations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/99") {
					confirmations++
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if test.status == http.StatusOK && r.URL.Query().Get("page") == "1" {
					w.Header().Set("X-Next-Page", "2")
					fmt.Fprint(w, `[]`)
					return
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			jobs, err := New(server.URL, "secret").Sync(context.Background(), []int64{1}, []state.Job{{ProjectID: 1, ID: 99}})
			if err == nil || jobs != nil || confirmations != 0 || strings.Contains(err.Error(), "secret-data") {
				t.Fatalf("failed listing accepted deletion: %v %v, confirmations=%d", jobs, err, confirmations)
			}
			if test.status != http.StatusOK {
				var status *HTTPStatusError
				if !errors.As(err, &status) || status.StatusCode != test.status {
					t.Fatalf("HTTP status was not typed: %v", err)
				}
			}
		})
	}
}

func TestInvalidJobConfirmationsRejectSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, "secret-data"},
		{"forbidden", http.StatusForbidden, "secret-data"},
		{"rate limited", http.StatusTooManyRequests, "secret-data"},
		{"server failure", http.StatusInternalServerError, "secret-data"},
		{"malformed", http.StatusOK, `{`},
		{"unknown status", http.StatusOK, `{"id":99,"status":"future_status"}`},
		{"unexpected ID", http.StatusOK, `{"id":100,"status":"success"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/99") {
					w.Header().Set("Retry-After", "35")
					w.WriteHeader(test.status)
					fmt.Fprint(w, test.body)
					return
				}
				fmt.Fprint(w, `[]`)
			}))
			defer server.Close()
			jobs, err := New(server.URL, "secret").Sync(context.Background(), []int64{1}, []state.Job{{ProjectID: 1, ID: 99}})
			if err == nil || jobs != nil || strings.Contains(err.Error(), "secret-data") {
				t.Fatalf("invalid confirmation accepted: %v %v", jobs, err)
			}
			if test.status == http.StatusTooManyRequests {
				var limit *RateLimitError
				if !errors.As(err, &limit) || limit.Delay != 35*time.Second {
					t.Fatalf("confirmation rate limit lost: %v", err)
				}
			}
		})
	}
}

func TestConfirmationOutsideConfiguredProjectsMakesNoRequests(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	_, err := New(server.URL, "secret").Sync(context.Background(), []int64{1}, []state.Job{{ProjectID: 2, ID: 99}})
	if err == nil || calls != 0 {
		t.Fatalf("unconfigured project reached GitLab: %v, calls=%d", err, calls)
	}
}
