package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/controller"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

type Backend interface {
	Hint(state.Hint) error
	Status() controller.Status
	MetricsHandler() http.Handler
}

func Handler(cfg config.Config, backend Backend, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !backend.Status().Ready {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	admin := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.AdminToken == "" {
				http.NotFound(w, r)
				return
			}
			if !equal(r.Header.Get("Authorization"), "Bearer "+cfg.AdminToken) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
	mux.Handle("GET /status", admin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(backend.Status()); err != nil {
			logger.Debug("status response interrupted", "error", err)
		}
	})))
	mux.Handle("GET /metrics", admin(backend.MetricsHandler()))
	mux.HandleFunc("POST /webhook/gitlab", func(w http.ResponseWriter, r *http.Request) {
		if !equal(r.Header.Get("X-Gitlab-Token"), cfg.WebhookSecret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Gitlab-Event") != "Job Hook" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "" {
			mediaType, _, err := mime.ParseMediaType(contentType)
			if err != nil || mediaType != "application/json" {
				http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var event struct {
			Kind      string `json:"object_kind"`
			ProjectID int64  `json:"project_id"`
			Project   struct {
				ID int64 `json:"id"`
			} `json:"project"`
			ID         int64  `json:"build_id"`
			PipelineID int64  `json:"pipeline_id"`
			Status     string `json:"build_status"`
		}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&event); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
			}
			return
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			http.Error(w, "expected one JSON value", http.StatusBadRequest)
			return
		}
		if event.ProjectID == 0 {
			event.ProjectID = event.Project.ID
		}
		if event.Kind != "build" || event.ID <= 0 || event.PipelineID <= 0 {
			http.Error(w, "invalid job event", http.StatusBadRequest)
			return
		}
		if !slices.Contains(cfg.ProjectIDs, event.ProjectID) {
			http.Error(w, "project is not configured", http.StatusForbidden)
			return
		}
		if event.Status == "cancelled" {
			event.Status = "canceled"
		}
		if !slices.Contains([]string{"pending", "preparing", "running", "canceling", "success", "failed", "canceled", "skipped", "manual", "created", "scheduled", "waiting_for_resource", "waiting_for_callback"}, event.Status) {
			http.Error(w, "unknown job status", http.StatusBadRequest)
			return
		}
		if err := backend.Hint(state.Hint{ProjectID: event.ProjectID, ID: event.ID, Status: event.Status}); err != nil {
			logger.Error("could not persist job hint", "error", err)
			http.Error(w, "controller unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func Server(address string, handler http.Handler) *http.Server {
	return &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}
