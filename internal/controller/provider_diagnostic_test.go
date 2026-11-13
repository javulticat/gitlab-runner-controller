//go:build linux || darwin

package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/commandprovider"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type diagnosticLogWriter struct {
	records chan string
}

func (w diagnosticLogWriter) Write(data []byte) (int, error) {
	w.records <- string(data)
	return len(data), nil
}

func TestProviderDiagnosticsReachStatusAndLogs(t *testing.T) {
	for _, mode := range []string{"account", "slot"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t, testPool("linux", 1, 1, 0))
			f.api.jobs = []state.Job{job(1, "pending", "linux")}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			client, err := commandprovider.New([]string{executable, "-test.run=^TestDiagnosticAdapterProcess$"},
				[]string{"GRC_DIAGNOSTIC_HELPER=" + mode, "API_TOKEN=private-credential", "GORACE=atexit_sleep_ms=0"},
				f.host.description, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			f.c.providers["host"] = client
			records := make(chan string, 16)
			f.c.log = slog.New(slog.NewJSONHandler(diagnosticLogWriter{records: records}, nil))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- f.c.Run(ctx) }()
			var record string
			select {
			case record = <-records:
			case <-time.After(10 * time.Second):
				t.Fatal("provider failure did not produce a log")
			}
			cancel()
			if err := runResult(t, done); err != nil {
				t.Fatal(err)
			}
			status := f.c.Status()
			message := status.State.ProviderErrors["host"]
			if mode == "slot" {
				message = status.State.Pools["linux"].Slots[0].LastError
			} else if status.LastError == "" {
				t.Fatal("account failure missing from reconciliation status")
			}
			for _, output := range []string{message, record} {
				if !strings.Contains(output, "(invalid)") || !strings.Contains(output, "call=hosts.create http_status=403 code=AccessDenied") {
					t.Fatalf("failure omitted kind or diagnostic: %s", output)
				}
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private-credential", f.cfg.Pools[0].Token, "raw-provider-body"} {
				if strings.Contains(string(encoded)+record, private) {
					t.Fatal("status or logs exposed private provider data")
				}
			}
			recorder := httptest.NewRecorder()
			f.c.MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if strings.Contains(recorder.Body.String(), "AccessDenied") || strings.Contains(recorder.Body.String(), "hosts.create") {
				t.Fatal("diagnostic entered metric labels")
			}
		})
	}
}

func TestDiagnosticAdapterProcess(t *testing.T) {
	mode := os.Getenv("GRC_DIAGNOSTIC_HELPER")
	if mode == "" {
		return
	}
	instance := newFakeProvider(provider.Retained)
	failure := &provider.Error{Kind: provider.Invalid, NoSideEffect: true, Detail: "raw-provider-body private-credential glrt-test-linux",
		Diagnostic: &provider.Diagnostic{Operation: "hosts.create", HTTPStatus: 403, Code: "AccessDenied"}}
	if mode == "account" {
		instance.describeErr = failure
	} else {
		instance.createErr = failure
	}
	if err := provider.Serve(context.Background(), os.Stdin, os.Stdout, func() (provider.Provider, error) { return instance, nil }); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
