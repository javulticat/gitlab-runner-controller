package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckConfigHasNoStateOrNetworkSideEffects(t *testing.T) {
	data, err := os.ReadFile("../../controller.example.json")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "should-not-exist")
	t.Setenv("STORAGE_DIR", stateDir)
	t.Setenv("GITLAB_API_TOKEN", "test-api-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", strings.Repeat("w", 32))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command, err := json.Marshal([]string{executable, "must-not-run"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_A_COMMAND_JSON", string(command))
	t.Setenv("HOST_A_ENDPOINT", "https://api.example.com")
	t.Setenv("HOST_A_TOKEN", "test-provider-token")
	t.Setenv("HOST_A_ACCOUNT_ID", "account-example")
	t.Setenv("HOST_A_LIFECYCLE", "retained")
	t.Setenv("GITLAB_RUNNER_LINUX_TOKEN", "glrt-linux-test")
	t.Setenv("GITLAB_RUNNER_PROTECTED_TOKEN", "glrt-protected-test")
	t.Setenv("CONTROLLER_CONFIG_JSON", string(data))
	t.Setenv("ADMIN_TOKEN", "")
	var output bytes.Buffer
	if err := execute(context.Background(), []string{"check-config", "--config", "nonexistent.json"}, &output, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "configuration is valid") {
		t.Fatal("check-config did not confirm validation")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatal("check-config created state")
	}
}

func TestHealthcheckUsesRealHTTP(t *testing.T) {
	for _, status := range []int{200, 503} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		err := healthcheck(context.Background(), server.URL)
		server.Close()
		if (status != 200) != (err != nil) {
			t.Fatalf("health %d: %v", status, err)
		}
	}
}

func TestVersionAndUnknownCommand(t *testing.T) {
	var output bytes.Buffer
	if err := execute(context.Background(), []string{"version"}, &output, slog.Default()); err != nil || output.String() != buildVersion()+"\n" {
		t.Fatalf("version: %s %v", output.String(), err)
	}
	if err := execute(context.Background(), []string{"unknown"}, io.Discard, slog.Default()); err == nil {
		t.Fatal("unknown command accepted")
	}
}

type testControllerService struct {
	run func(context.Context) error
}

func (s testControllerService) Run(ctx context.Context) error { return s.run(ctx) }
func (testControllerService) ShutdownGrace() time.Duration    { return time.Second }

func awaitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

func awaitServe(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("server did not finish shutdown")
		return nil
	}
}

func TestServeDrainsHTTPAndJoinsActiveController(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	})}
	shutdownStarted := make(chan struct{})
	server.RegisterOnShutdown(func() { close(shutdownStarted) })
	controllerStarted := make(chan struct{})
	controllerStopping := make(chan struct{})
	releaseController := make(chan struct{})
	service := testControllerService{run: func(ctx context.Context) error {
		close(controllerStarted)
		<-ctx.Done()
		close(controllerStopping)
		<-releaseController
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, service, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	awaitSignal(t, controllerStarted, "controller did not start")
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		response, requestErr := client.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			requestErr = response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				requestErr = errors.Join(requestErr, errors.New("HTTP drain interrupted the active response"))
			}
		}
		requestDone <- requestErr
	}()
	awaitSignal(t, requestStarted, "HTTP request did not start")
	cancel()
	awaitSignal(t, controllerStopping, "shutdown did not stop controller admission")
	awaitSignal(t, shutdownStarted, "HTTP shutdown did not start")
	select {
	case err := <-done:
		t.Fatalf("server returned before active work finished: %v", err)
	default:
	}
	close(releaseRequest)
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active HTTP request did not drain")
	}
	select {
	case err := <-done:
		t.Fatalf("server returned before controller finished: %v", err)
	default:
	}
	close(releaseController)
	if err := awaitServe(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestServeControllerFailureClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("controller failed")
	service := testControllerService{run: func(context.Context) error { return want }}
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), &http.Server{}, listener, service, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	if err := awaitServe(t, done); !errors.Is(err, want) {
		t.Fatalf("controller error was lost: %v", err)
	}
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err == nil {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("controller failure left the listener open")
	}
}

func TestServeListenerFailureCancelsAndJoinsController(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	service := testControllerService{run: func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return nil
	}}
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), &http.Server{}, listener, service, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	if err := awaitServe(t, done); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener error was lost: %v", err)
	}
	awaitSignal(t, stopped, "listener failure left the controller running")
}
