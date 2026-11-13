package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func policyRequestID(t *testing.T, r *http.Request) int64 {
	t.Helper()
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer secret" {
		t.Errorf("invalid GraphQL request: method=%s, content-type=%s", r.Method, r.Header.Get("Content-Type"))
	}
	var body struct {
		Query     string `json:"query"`
		Variables struct {
			ID string `json:"id"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("invalid GraphQL request body: %v", err)
		return 0
	}
	for _, field := range []string{"runner(id: $id)", "id", "paused", "accessLevel", "runUntagged", "tagList"} {
		if !strings.Contains(body.Query, field) {
			t.Errorf("runner query omitted %s", field)
		}
	}
	if strings.Contains(body.Query, "mutation") || strings.Contains(body.Query, "Token") {
		t.Error("runner policy query requested a mutation or token")
	}
	if !strings.HasPrefix(body.Variables.ID, "gid://gitlab/Ci::Runner/") {
		t.Errorf("invalid runner global ID: %s", body.Variables.ID)
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(body.Variables.ID, "gid://gitlab/Ci::Runner/"), 10, 64)
	if err != nil {
		t.Errorf("invalid runner numeric ID: %v", err)
	}
	return id
}

func writeRunnerPolicy(t *testing.T, w http.ResponseWriter, id int64) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"runner": map[string]any{
		"id": fmt.Sprintf("gid://gitlab/Ci::Runner/%d", id), "paused": false,
		"accessLevel": "NOT_PROTECTED", "runUntagged": false, "tagList": []string{"linux"},
	}}}); err != nil {
		t.Error(err)
	}
}

func TestRunnerPoliciesAndCompleteInheritedAvailability(t *testing.T) {
	metadataCalls := 0
	availabilityCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gitlab/api/graphql" {
			metadataCalls++
			id := policyRequestID(t, r)
			if id == 2 {
				fmt.Fprint(w, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/2","paused":true,"accessLevel":"REF_PROTECTED","runUntagged":true,"tagList":[]}}}`)
			} else {
				writeRunnerPolicy(t, w, id)
			}
			return
		}
		availabilityCalls++
		if r.Method != http.MethodGet || r.Header.Get("PRIVATE-TOKEN") != "secret" || r.URL.Query().Get("per_page") != "100" || len(r.URL.Query()) != 2 {
			t.Errorf("project availability was filtered or unauthorized: %s", r.URL.RequestURI())
		}
		switch r.URL.Path {
		case "/gitlab/api/v4/projects/10/runners":
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				fmt.Fprint(w, `[{"id":1,"runner_type":"group_type","online":false,"status":"offline"}]`)
			} else {
				fmt.Fprint(w, `[{"id":2,"runner_type":"instance_type","online":false,"status":"never_contacted"}]`)
			}
		case "/gitlab/api/v4/projects/20/runners":
			fmt.Fprint(w, `[{"id":1,"runner_type":"project_type","online":false,"status":"stale"}]`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	policies, err := New(server.URL+"/gitlab/", "secret").Policies(context.Background(), []int64{10, 20, 10}, []int64{1, 2, 1})
	if err != nil {
		t.Fatal(err)
	}
	one, two := policies[1], policies[2]
	if len(policies) != 2 || metadataCalls != 2 || availabilityCalls != 3 {
		t.Fatalf("duplicate requests or missing policies: %+v, metadata=%d availability=%d", policies, metadataCalls, availabilityCalls)
	}
	if one.ID != 1 || !one.Known || one.Paused || one.ProtectedOnly || one.RunUntagged || !slices.Equal(one.Tags, []string{"linux"}) || !one.Projects[10] || !one.Projects[20] || one.Error != "" {
		t.Fatalf("offline runner lost eligibility: %+v", one)
	}
	if two.ID != 2 || !two.Known || !two.Paused || !two.ProtectedOnly || !two.RunUntagged || len(two.Tags) != 0 || !two.Projects[10] || two.Error != "" {
		t.Fatalf("protected paused runner policy is incorrect: %+v", two)
	}
	if available, known := two.Projects[20]; !known || available {
		t.Fatalf("complete absence was not recorded: %+v", two)
	}
}

func TestRunnerMetadataFailuresStayIsolatedAndSafe(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, "secret-data"},
		{"forbidden", http.StatusForbidden, "secret-data"},
		{"missing", http.StatusNotFound, "secret-data"},
		{"server failure", http.StatusInternalServerError, "secret-data"},
		{"malformed JSON", http.StatusOK, `{secret-data`},
		{"null data", http.StatusOK, `{"data":null}`},
		{"null runner", http.StatusOK, `{"data":{"runner":null}}`},
		{"GraphQL errors", http.StatusOK, `{"data":{"runner":null},"errors":[{"message":"secret-data","extensions":{"token":"secret-data"}}]}`},
		{"partial field error", http.StatusOK, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/1","paused":false,"accessLevel":"NOT_PROTECTED","runUntagged":false,"tagList":["linux"]}},"errors":[{"message":"secret-data","path":["runner","tagList"]}]}`},
		{"wrong actual ID", http.StatusOK, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/99","paused":false,"accessLevel":"NOT_PROTECTED","runUntagged":false,"tagList":["linux"]}}}`},
		{"missing paused", http.StatusOK, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/1","accessLevel":"NOT_PROTECTED","runUntagged":false,"tagList":["linux"]}}}`},
		{"missing untagged", http.StatusOK, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/1","paused":false,"accessLevel":"NOT_PROTECTED","tagList":["linux"]}}}`},
		{"unknown access level", http.StatusOK, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/1","paused":false,"accessLevel":"FUTURE_LEVEL","runUntagged":false,"tagList":["linux"]}}}`},
		{"null tags", http.StatusOK, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/1","paused":false,"accessLevel":"NOT_PROTECTED","runUntagged":false,"tagList":null}}}`},
		{"invalid tag", http.StatusOK, `{"data":{"runner":{"id":"gid://gitlab/Ci::Runner/1","paused":false,"accessLevel":"NOT_PROTECTED","runUntagged":false,"tagList":[null]}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/graphql" {
					id := policyRequestID(t, r)
					if id == 1 {
						w.WriteHeader(test.status)
						fmt.Fprint(w, test.body)
					} else {
						writeRunnerPolicy(t, w, id)
					}
					return
				}
				fmt.Fprint(w, `[{"id":1},{"id":2}]`)
			}))
			defer server.Close()
			policies, err := New(server.URL, "secret").Policies(context.Background(), []int64{10}, []int64{1, 2})
			if err != nil || policies[1].Known || policies[1].ID != 1 || policies[1].Error == "" || !policies[2].Known || policies[2].Error != "" {
				t.Fatalf("metadata failure was not isolated: %+v %v", policies, err)
			}
			if !policies[1].Projects[10] || !policies[2].Projects[10] || strings.Contains(fmt.Sprint(policies), "secret-data") {
				t.Fatalf("unknown metadata lost availability or exposed response data: %+v", policies)
			}
			if !strings.Contains(policies[1].Error, "runner 1 metadata query failed") {
				t.Fatalf("metadata failure lost request context: %q", policies[1].Error)
			}
			if test.status != http.StatusOK && !strings.Contains(policies[1].Error, fmt.Sprintf("HTTP %d", test.status)) {
				t.Fatalf("metadata failure lost HTTP status: %q", policies[1].Error)
			}
		})
	}
}

func TestIncompleteProjectRunnerListsRemainUnknown(t *testing.T) {
	for _, test := range []struct {
		name, body, next string
		status           int
	}{
		{"unauthorized", "secret-data", "", http.StatusUnauthorized},
		{"forbidden", "secret-data", "", http.StatusForbidden},
		{"missing project", "secret-data", "", http.StatusNotFound},
		{"server failure", "secret-data", "", http.StatusInternalServerError},
		{"malformed", `{`, "", http.StatusOK},
		{"null", `null`, "", http.StatusOK},
		{"invalid ID", `[{"id":0}]`, "", http.StatusOK},
		{"duplicate ID", `[{"id":1}]`, "", http.StatusOK},
		{"invalid pagination", `[]`, "9", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/graphql" {
					writeRunnerPolicy(t, w, policyRequestID(t, r))
					return
				}
				if r.URL.Path != "/api/v4/projects/20/runners" {
					fmt.Fprint(w, `[{"id":1}]`)
					return
				}
				if r.URL.Query().Get("page") == "1" {
					w.Header().Set("X-Next-Page", "2")
					fmt.Fprint(w, `[{"id":1}]`)
					return
				}
				w.Header().Set("X-Next-Page", test.next)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			policies, err := New(server.URL, "secret").Policies(context.Background(), []int64{10, 20, 30}, []int64{1, 2})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []int64{1, 2} {
				policy := policies[id]
				if !policy.Known || policy.Error == "" || strings.Contains(policy.Error, "secret-data") {
					t.Fatalf("availability failure lost metadata or response safety: %+v", policy)
				}
				if !strings.Contains(policy.Error, "project 20 runner availability check failed") {
					t.Fatalf("availability failure lost project context: %q", policy.Error)
				}
				if test.status != http.StatusOK && !strings.Contains(policy.Error, fmt.Sprintf("HTTP %d", test.status)) {
					t.Fatalf("availability failure lost HTTP status: %q", policy.Error)
				}
				if _, known := policy.Projects[20]; known {
					t.Fatalf("partial page became known availability: %+v", policy)
				}
				for _, project := range []int64{10, 30} {
					if available, known := policy.Projects[project]; !known || available != (id == 1) {
						t.Fatalf("independent project availability was lost: %+v", policy)
					}
				}
			}
		})
	}
}

func TestRunnerPolicyRateLimitsRejectRefresh(t *testing.T) {
	for _, limitedPath := range []string{"/api/graphql", "/api/v4/projects/10/runners"} {
		t.Run(limitedPath, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == limitedPath {
					w.Header().Set("Retry-After", "75")
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, "secret-data")
				} else {
					writeRunnerPolicy(t, w, policyRequestID(t, r))
				}
			}))
			defer server.Close()
			policies, err := New(server.URL, "secret").Policies(context.Background(), []int64{10}, []int64{1})
			var limit *RateLimitError
			if policies != nil || !errors.As(err, &limit) || limit.Delay != 75*time.Second || strings.Contains(err.Error(), "secret-data") {
				t.Fatalf("runner rate limit was lost: %+v %v", policies, err)
			}
		})
	}
}

func TestRunnerPoliciesRespectBoundsAndRedirectPolicy(t *testing.T) {
	for _, mode := range []string{"oversized metadata", "metadata redirect", "availability redirect"} {
		t.Run(mode, func(t *testing.T) {
			redirected := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected = true
					fmt.Fprint(w, `[]`)
					return
				}
				if r.URL.Path == "/api/graphql" {
					id := policyRequestID(t, r)
					switch mode {
					case "oversized metadata":
						fmt.Fprint(w, strings.Repeat(" ", 4<<20+1))
					case "metadata redirect":
						http.Redirect(w, r, "/redirected", http.StatusFound)
					default:
						writeRunnerPolicy(t, w, id)
					}
					return
				}
				if mode == "availability redirect" {
					http.Redirect(w, r, "/redirected", http.StatusFound)
				} else {
					fmt.Fprint(w, `[{"id":1}]`)
				}
			}))
			defer server.Close()
			policies, err := New(server.URL, "secret").Policies(context.Background(), []int64{10}, []int64{1})
			if err != nil || redirected || policies[1].Error == "" {
				t.Fatalf("response bound or redirect policy failed: %+v %v redirected=%t", policies, err, redirected)
			}
			if mode == "availability redirect" {
				if _, known := policies[1].Projects[10]; known || !policies[1].Known {
					t.Fatalf("redirect made availability known or lost metadata: %+v", policies[1])
				}
			} else if policies[1].Known || !policies[1].Projects[10] {
				t.Fatalf("metadata failure affected independent availability: %+v", policies[1])
			}
		})
	}
}

func TestRunnerPoliciesWithoutValidIDsMakeNoRequests(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	client := New(server.URL, "secret")
	for _, ids := range [][]int64{nil, {0, -1, 0}} {
		policies, err := client.Policies(context.Background(), []int64{10}, ids)
		if err != nil || calls != 0 {
			t.Fatalf("invalid runner IDs reached GitLab: %+v %v, calls=%d", policies, err, calls)
		}
		for _, policy := range policies {
			if policy.Known || policy.Error == "" || len(policy.Projects) != 0 {
				t.Fatalf("invalid runner ID became known: %+v", policy)
			}
		}
	}
}
