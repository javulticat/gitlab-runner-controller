package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type shutdownProvider struct {
	*fakeProvider
	create   func(context.Context, provider.CreateRequest) (provider.Resource, error)
	describe func(context.Context) (provider.Description, error)
}

func (p *shutdownProvider) Create(ctx context.Context, request provider.CreateRequest) (provider.Resource, error) {
	if p.create != nil {
		return p.create(ctx, request)
	}
	return p.fakeProvider.Create(ctx, request)
}

func (p *shutdownProvider) Describe(ctx context.Context) (provider.Description, error) {
	if p.describe != nil {
		return p.describe(ctx)
	}
	return p.fakeProvider.Describe(ctx)
}

func waitForStopping(t *testing.T, c *Controller) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for !c.stopping.Load() {
		select {
		case <-timeout.C:
			t.Fatal("controller did not stop admission")
		default:
			runtime.Gosched()
		}
	}
}

func runResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not finish shutdown")
		return nil
	}
}

func TestRunAlreadyCanceledStartsNoRequests(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if f.api.calls != 0 || f.host.describes != 0 || f.host.lists != 0 || f.host.mutations() != 0 {
		t.Fatal("canceled startup contacted an external service")
	}
	if !f.c.stopping.Load() {
		t.Fatal("canceled startup did not stop admission")
	}
}

func TestAdmissionCancellationBetweenMutationsStopsNextDispatch(t *testing.T) {
	f := setup(t, testPool("linux", 2, 1, 0))
	f.api.jobs = []state.Job{job(1, "pending", "linux"), job(2, "pending", "linux")}
	admission, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := context.WithValue(context.Background(), admissionContextKey{}, admission)
	f.host.onCreate = cancel
	if err := f.c.Step(work); !errors.Is(err, ErrStopping) {
		t.Fatalf("canceled admission did not stop reconciliation: %v", err)
	}
	if f.host.creates != 1 || f.host.persists != 0 || f.host.configures != 0 {
		t.Fatal("canceled admission dispatched another mutation")
	}
	if f.store.Snapshot().Pools["linux"].Slots[0].ResourceID == "" {
		t.Fatal("admission cancellation lost the completed create result")
	}
}

func TestRunShutdownSavesLateCreateResultAndStartsNoNextMutation(t *testing.T) {
	f := setup(t, testPool("linux", 2, 1, 0))
	f.api.jobs = []state.Job{job(1, "pending", "linux"), job(2, "pending", "linux")}
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	f.c.providers["host"] = &shutdownProvider{fakeProvider: f.host, create: func(ctx context.Context, request provider.CreateRequest) (provider.Resource, error) {
		started <- ctx
		<-release
		return f.host.Create(ctx, request)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.c.Run(ctx) }()
	var active context.Context
	select {
	case active = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not start")
	}
	cancel()
	waitForStopping(t, f.c)
	if err := active.Err(); err != nil {
		t.Fatalf("shutdown canceled the active create: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown returned before create completed: %v", err)
	default:
	}
	close(release)
	if err := runResult(t, done); err != nil {
		t.Fatal(err)
	}
	if f.host.creates != 1 || f.host.persists != 0 || f.host.configures != 0 {
		t.Fatal("shutdown started another lifecycle operation")
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved state.State
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	slot := saved.Pools["linux"].Slots[0]
	if slot.ResourceID == "" || slot.Operation != nil && (slot.Operation.Kind == "create" || slot.Operation.Kind == "unresolved") {
		t.Fatal("late create result was not saved durably")
	}
	if err := f.c.Step(context.Background()); !errors.Is(err, ErrStopping) {
		t.Fatalf("stopped controller accepted another reconciliation: %v", err)
	}
}

func TestRunShutdownStopsAfterActiveReadOnlyRequest(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	f.c.providers["host"] = &shutdownProvider{fakeProvider: f.host, describe: func(ctx context.Context) (provider.Description, error) {
		started <- ctx
		<-release
		return f.host.Describe(ctx)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.c.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("describe did not start")
	}
	cancel()
	waitForStopping(t, f.c)
	close(release)
	if err := runResult(t, done); err != nil {
		t.Fatal(err)
	}
	if f.host.lists != 0 || f.host.mutations() != 0 {
		t.Fatal("shutdown continued provider work after the active request")
	}
}

func TestRunShutdownPreservesStorageFailure(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	started := make(chan struct{})
	release := make(chan struct{})
	f.c.providers["host"] = &shutdownProvider{fakeProvider: f.host, create: func(ctx context.Context, request provider.CreateRequest) (provider.Resource, error) {
		close(started)
		<-release
		if err := os.Rename(filepath.Dir(f.path), filepath.Dir(f.path)+"-unavailable"); err != nil {
			return provider.Resource{}, err
		}
		return f.host.Create(ctx, request)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.c.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not start")
	}
	cancel()
	waitForStopping(t, f.c)
	close(release)
	if err := runResult(t, done); !errors.Is(err, state.ErrPoisoned) {
		t.Fatalf("shutdown hid a failed durable write: %v", err)
	}
}

func TestRunShutdownForcesCancellationAndJoinsActiveRequest(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.c.cfg.RequestTimeout = config.Duration(time.Second)
	started := make(chan struct{})
	finished := make(chan struct{})
	f.c.providers["host"] = &shutdownProvider{fakeProvider: f.host, describe: func(ctx context.Context) (provider.Description, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return provider.Description{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.c.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("describe did not start")
	}
	cancel()
	if err := runResult(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before the canceled request exited")
	}
	if f.host.lists != 0 || f.host.mutations() != 0 {
		t.Fatal("forced shutdown admitted new provider work")
	}
}

func TestShutdownGraceIncludesRequestAndCleanupBudgets(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.c.cfg.RequestTimeout = config.Duration(2 * time.Minute)
	if got, want := f.c.ShutdownGrace(), 2*time.Minute+10*time.Second; got != want {
		t.Fatalf("shutdown grace = %s, want %s", got, want)
	}
}
