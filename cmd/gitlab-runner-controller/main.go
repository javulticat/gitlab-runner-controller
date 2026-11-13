package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/controller"
	"github.com/javulticat/gitlab-runner-controller/internal/gitlab"
	"github.com/javulticat/gitlab-runner-controller/internal/httpapi"
	"github.com/javulticat/gitlab-runner-controller/internal/httpclient"
	"github.com/javulticat/gitlab-runner-controller/internal/providers"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := execute(ctx, os.Args[1:], os.Stdout, logger); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logger.Error("controller exited", "error", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, args []string, out io.Writer, logger *slog.Logger) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command == "version" {
		_, err := fmt.Fprintln(out, buildVersion())
		return err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", envDefault("CONTROLLER_CONFIG", "controller.json"), "path to the controller JSON configuration")
	pool := flags.String("pool", "", "pool for an offline recovery command")
	slot := flags.Int("slot", -1, "zero-based slot index for recovery")
	id := flags.String("resource-id", "", "provider resource found after an unresolved creation")
	absent := flags.Bool("confirm-absent", false, "confirm provider audit found no resource for the unresolved creation")
	projectID := flags.Int64("project-id", 0, "project ID of an exhausted pending job")
	jobID := flags.Int64("job-id", 0, "job ID of an exhausted pending job")
	confirmRetry := flags.Bool("confirm-retry", false, "authorize one additional bounded pending-job attempt")
	probe := flags.String("url", "http://127.0.0.1:"+envDefault("PORT", "3000")+"/healthz", "local health URL (healthcheck only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "healthcheck" {
		return healthcheck(ctx, *probe)
	}
	if command != "serve" && command != "check-config" && command != "resolve-create" && command != "release-missing" && command != "retry-pending" {
		return errors.New("unknown command; use serve, check-config, healthcheck, resolve-create, release-missing, retry-pending, or version")
	}
	cfg, err := config.Load(*path, os.Getenv)
	if err != nil {
		return err
	}
	registry, err := providers.Build(cfg)
	if err != nil {
		return err
	}
	if command == "check-config" {
		_, err := fmt.Fprintln(out, "configuration is valid")
		return err
	}
	open := state.Open
	if command == "release-missing" || command == "retry-pending" {
		open = state.OpenForRecovery
	}
	store, err := open(cfg.StatePath, cfg.ControllerID, controller.Binding(cfg))
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("state lock close failed", "error", err)
		}
	}()
	if command == "release-missing" {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		backup, err := controller.ReleaseMissing(ctx, cfg, store, registry, *pool, *slot, *id, *absent)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "missing resource released after audit; original snapshot backed up at "+backup)
		return err
	}
	if command == "retry-pending" {
		backup, err := controller.RetryPending(cfg, store, *pool, *projectID, *jobID, *confirmRetry)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "one additional pending-job attempt authorized; original snapshot backed up at "+backup)
		return err
	}
	service, err := controller.New(cfg, store, gitlab.New(cfg.GitLabURL, cfg.GitLabToken), registry, logger)
	if err != nil {
		return err
	}
	if command == "resolve-create" {
		if err := service.ResolveCreate(*pool, *slot, *id, *absent); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "creation resolved; restart the controller to verify provider inventory")
		return err
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := httpapi.Server(cfg.ListenAddr, httpapi.Handler(cfg, service, logger))
	return serve(ctx, server, listener, service, logger)
}

const httpShutdownGrace = 20 * time.Second

type controllerService interface {
	Run(context.Context) error
	ShutdownGrace() time.Duration
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, service controllerService, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	serverDone := make(chan error, 1)
	controllerDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	go func() { controllerDone <- service.Run(ctx) }()
	logger.Info("controller started", "address", listener.Addr().String(), "version", buildVersion())
	var runErr error
	controllerStopped := false
	serverStopped := false
	select {
	case <-ctx.Done():
	case runErr = <-serverDone:
		serverStopped = true
		if errors.Is(runErr, http.ErrServerClosed) {
			runErr = nil
		}
	case runErr = <-controllerDone:
		controllerStopped = true
	}
	cancel()
	logger.Info("controller stopping", "shutdown_grace", service.ShutdownGrace()+httpShutdownGrace)
	shutdown, stop := context.WithTimeout(context.Background(), httpShutdownGrace)
	defer stop()
	if err := server.Shutdown(shutdown); err != nil {
		runErr = errors.Join(runErr, err, server.Close())
	}
	if !serverStopped {
		if err := <-serverDone; !errors.Is(err, http.ErrServerClosed) {
			runErr = errors.Join(runErr, err)
		}
	}
	if !controllerStopped {
		runErr = errors.Join(runErr, <-controllerDone)
	}
	logger.Info("controller stopped")
	return runErr
}

func healthcheck(ctx context.Context, address string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("invalid health URL")
	}
	response, err := httpclient.New(3 * time.Second).Do(req)
	if err != nil {
		return errors.New("health request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	return nil
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
