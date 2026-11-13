package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/controller"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

type fakeBackend struct {
	hints []state.Hint
	ready bool
	err   error
}

func (f *fakeBackend) Hint(h state.Hint) error   { f.hints = append(f.hints, h); return f.err }
func (f *fakeBackend) Status() controller.Status { return controller.Status{Ready: f.ready} }
func (*fakeBackend) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(w, "test_metric 1\n"); err != nil {
			panic(err)
		}
	})
}

func apiFixture() (config.Config, *fakeBackend, http.Handler) {
	cfg := config.Defaults()
	cfg.ProjectIDs = []int64{1}
	cfg.AdminToken, cfg.WebhookSecret = strings.Repeat("a", 32), strings.Repeat("w", 32)
	backend := &fakeBackend{}
	return cfg, backend, Handler(cfg, backend, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestAdminIsolationAndReadiness(t *testing.T) {
	cfg, backend, handler := apiFixture()
	for _, path := range []string{"/status", "/metrics"} {
		for _, token := range []string{"", "Bearer " + cfg.WebhookSecret, "Bearer " + cfg.AdminToken} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", token)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			want := 401
			if token == "Bearer "+cfg.AdminToken {
				want = 200
			}
			if recorder.Code != want {
				t.Fatalf("%s authorization: %d", path, recorder.Code)
			}
		}
	}
	for _, ready := range []bool{false, true} {
		backend.ready = ready
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		want := 503
		if ready {
			want = 200
		}
		if response.Code != want {
			t.Fatalf("readiness: %d", response.Code)
		}
	}
	cfg.AdminToken = ""
	handler = Handler(cfg, backend, slog.Default())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	if response.Code != 404 {
		t.Fatal("disabled admin route was exposed")
	}
}

func TestJobHookValidationAndDurableAcknowledgment(t *testing.T) {
	valid := `{"object_kind":"build","project_id":1,"build_id":2,"pipeline_id":3,"build_status":"pending","extra_gitlab_field":true}`
	for _, test := range []struct {
		name, body, token, contentType, event string
		storageErr                            bool
		want                                  int
	}{
		{"valid", valid, "valid", "application/json", "Job Hook", false, 202},
		{"bad auth", valid, "bad", "application/json", "Job Hook", false, 401},
		{"other event", "ignored", "valid", "", "Push Hook", false, 202},
		{"bad media", valid, "valid", "text/plain", "Job Hook", false, 415},
		{"bad JSON", "{", "valid", "application/json", "Job Hook", false, 400},
		{"multiple JSON", valid + " {}", "valid", "application/json", "Job Hook", false, 400},
		{"wrong project", strings.Replace(valid, `"project_id":1`, `"project_id":9`, 1), "valid", "application/json", "Job Hook", false, 403},
		{"bad status", strings.Replace(valid, "pending", "future", 1), "valid", "application/json", "Job Hook", false, 400},
		{"storage failed", valid, "valid", "application/json", "Job Hook", true, 503},
		{"oversized", `{"padding":"` + strings.Repeat("x", 1<<20) + `"}`, "valid", "application/json", "Job Hook", false, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, backend, handler := apiFixture()
			if test.storageErr {
				backend.err = errors.New("disk unavailable")
			}
			request := httptest.NewRequest(http.MethodPost, "/webhook/gitlab", strings.NewReader(test.body))
			token := test.token
			if token == "valid" {
				token = cfg.WebhookSecret
			}
			request.Header.Set("X-Gitlab-Token", token)
			request.Header.Set("X-Gitlab-Event", test.event)
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("got %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
			if test.name == "valid" && (len(backend.hints) != 1 || backend.hints[0].ID != 2) {
				t.Fatal("valid hint was lost")
			}
		})
	}
}

func TestMethodRestrictionsAndHealth(t *testing.T) {
	_, _, handler := apiFixture()
	for _, test := range []struct {
		method, path string
		want         int
	}{{"GET", "/healthz", 200}, {"GET", "/webhook/gitlab", 405}, {"POST", "/status", 405}, {"GET", "/missing", 404}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Fatalf("%s %s: %d", test.method, test.path, response.Code)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive routes can be cached")
		}
	}
}
