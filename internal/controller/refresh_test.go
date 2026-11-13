package controller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func TestRefreshDeferralPreservesHealthAndCompletedCycle(t *testing.T) {
	for _, lifecycle := range []provider.Lifecycle{provider.Retained, provider.Disposable} {
		t.Run(string(lifecycle), func(t *testing.T) {
			f := setupLifecycle(t, lifecycle, testPool("linux", 1, 1, 0))
			f.step(t, 0)
			before := f.c.Status()
			if !before.Ready {
				t.Fatal("initial reconciliation is not ready")
			}
			if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "pending"}); err != nil {
				t.Fatal(err)
			}
			if !f.c.Status().Ready || f.c.verified(f.store.Snapshot()) {
				t.Fatal("a pending hint must preserve health and block lifecycle admission")
			}
			saved := f.store.Snapshot()
			describes, lists := f.host.describes, f.host.lists
			f.step(t, time.Second)
			after := f.c.Status()
			if !after.Ready || after.LastError != "" || !after.LastStep.Equal(before.LastStep) {
				t.Fatalf("deferral changed completed reconciliation status: %+v", after)
			}
			if !reflect.DeepEqual(saved, f.store.Snapshot()) || f.host.describes != describes || f.host.lists != lists || f.host.mutations() != 0 || f.api.calls != 1 {
				t.Fatal("deferral wrote state or contacted a service")
			}
			families, err := f.c.metrics.registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range families {
				if family.GetName() == "grc_reconciliations_total" {
					if len(family.Metric) != 1 || family.Metric[0].Label[0].GetValue() != "success" || family.Metric[0].GetCounter().GetValue() != 1 {
						t.Fatalf("deferral counted as a completed cycle: %v", family)
					}
				}
			}
			f.now = f.now.Add(2 * time.Duration(f.cfg.JobsPollInterval))
			if f.c.Status().Ready {
				t.Fatal("stale synchronization remained ready")
			}
		})
	}
}

func TestRefreshDeferralPreservesFailureAndStartupGating(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.host.describeErr = errors.New("account unavailable")
	if err := f.c.Step(t.Context()); err == nil {
		t.Fatal("provider failure was ignored")
	}
	before := f.c.Status()
	if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	f.step(t, time.Second)
	if after := f.c.Status(); after.Ready || after.LastError != before.LastError || !after.LastStep.Equal(before.LastStep) {
		t.Fatal("deferral cleared a real failure")
	}
	f.restart(t)
	f.step(t, 0)
	if status := f.c.Status(); status.Ready || !status.LastStep.IsZero() || status.LastError != "" || f.api.calls != 1 {
		t.Fatal("restored observations bypassed startup synchronization")
	}
	f.host.describeErr = nil
	f.step(t, 4*time.Second)
	if !f.c.Status().Ready || f.api.calls != 2 {
		t.Fatal("startup failed to recover at the durable refresh deadline")
	}
}

func TestRevisionRaceDoesNotHideProviderFailure(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.step(t, 0)
	f.c.providers["host"] = &shutdownProvider{fakeProvider: f.host, describe: func(context.Context) (provider.Description, error) {
		if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "pending"}); err != nil {
			return provider.Description{}, err
		}
		return provider.Description{}, errors.New("account unavailable")
	}}
	if err := f.c.Step(t.Context()); err == nil || errors.Is(err, state.ErrStale) {
		t.Fatalf("revision race hid the account failure: %v", err)
	}
	if status := f.c.Status(); status.Ready || status.LastError == "" || status.State.ProviderErrors["host"] == "" {
		t.Fatal("revision race cleared provider failure status")
	}
}

func TestHintBeforeActivationDefersWithoutSlotFailure(t *testing.T) {
	for _, kind := range []string{"create", "resume"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, testPool("linux", 1, 1, 0))
			f.step(t, 0)
			f.c.cfg.ReadinessTimeout = config.Duration(time.Second)
			id := ""
			if kind == "resume" {
				id = "resource-1"
				f.host.resources[id] = provider.Resource{ID: id, State: provider.Stopped}
				if err := f.c.slotUpdate("linux", 0, func(slot *state.Slot) { slot.ResourceID = id }); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.c.begin("linux", 0, kind); err != nil {
				t.Fatal(err)
			}
			if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "pending"}); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			f.c.log = slog.New(slog.NewJSONHandler(&logs, nil))
			if err := f.c.activate(t.Context(), f.cfg.Pools[0], 0, id); err != state.ErrStale {
				t.Fatalf("unverified activation was not deferred: %v", err)
			}
			slot := f.store.Snapshot().Pools["linux"].Slots[0]
			f.now = f.now.Add(2 * time.Second)
			if slot.Operation != nil || slot.LastError != "" || !slot.RetryAt.IsZero() || !slot.BootstrapStartedAt.IsZero() || !slot.BootstrapDeadline.IsZero() || f.host.mutations() != 0 || logs.Len() != 0 || !f.c.Status().Ready {
				t.Fatalf("undispatched activation left a failure or uncertain intent: %+v", slot)
			}
			f.api.jobs = []state.Job{job(1, "pending", "linux")}
			f.step(t, 3*time.Second)
			if f.host.creates+f.host.resumes != 1 {
				t.Fatal("slot backoff delayed activation beyond refresh eligibility")
			}
		})
	}
}

func TestHintBeforeActivationPreservesPriorBootstrapFailure(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.step(t, 0)
	before := state.Slot{ResourceID: "resource-1", Power: provider.Stopped,
		BootstrapStartedAt: f.now.Add(-time.Minute), BootstrapDeadline: f.now.Add(time.Minute),
		LastError: "previous resume failed", RetryAt: f.now.Add(30 * time.Second)}
	if err := f.c.slotUpdate("linux", 0, func(slot *state.Slot) { *slot = before }); err != nil {
		t.Fatal(err)
	}
	if err := f.c.begin("linux", 0, "resume"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := f.c.activate(t.Context(), f.cfg.Pools[0], 0, before.ResourceID); err != state.ErrStale {
		t.Fatalf("unverified activation was not deferred: %v", err)
	}
	after := f.store.Snapshot().Pools["linux"].Slots[0]
	if after.Operation != nil || after.LastError != before.LastError || !after.RetryAt.Equal(before.RetryAt) ||
		!after.BootstrapStartedAt.Equal(before.BootstrapStartedAt) || !after.BootstrapDeadline.Equal(before.BootstrapDeadline) || f.host.resumes != 0 {
		t.Fatal("deferring an undispatched activation erased a prior real attempt")
	}
}

func TestRunRefreshDeadlineCoalescesHintsAndRetriesRaces(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		name := "deadline"
		if cancelWaiting {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := setup(t, testPool("linux", 1, 1, 0))
				f.c.now = time.Now
				f.c.cfg.PollInterval = config.Duration(time.Minute)
				var logs bytes.Buffer
				f.c.log = slog.New(slog.NewJSONHandler(&logs, nil))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if !cancelWaiting {
					f.api.onSync = func() {
						if f.api.calls == 2 {
							if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 3, Status: "pending"}); err != nil {
								t.Error(err)
							}
						}
					}
				}
				done := make(chan error, 1)
				go func() { done <- f.c.Run(ctx) }()
				synctest.Wait()
				if f.api.calls != 1 || !f.c.Status().Ready {
					t.Fatal("startup did not complete")
				}
				hint := func(id int64) {
					t.Helper()
					if err := f.c.Hint(state.Hint{ProjectID: 1, ID: id, Status: "pending"}); err != nil {
						t.Fatal(err)
					}
				}
				hint(1)
				synctest.Wait()
				time.Sleep(4 * time.Second)
				synctest.Wait()
				hint(2)
				synctest.Wait()
				if f.api.calls != 1 || !f.c.Status().Ready {
					t.Fatal("throttled hooks refreshed early or degraded readiness")
				}
				if !cancelWaiting {
					time.Sleep(time.Second)
					synctest.Wait()
					if f.api.calls != 2 || !f.c.Status().Ready || f.c.verified(f.store.Snapshot()) {
						t.Fatal("refresh missed its deadline or a racing hook changed readiness")
					}
					time.Sleep(5 * time.Second)
					synctest.Wait()
					if f.api.calls != 3 || !f.c.verified(f.store.Snapshot()) || len(f.store.Snapshot().Hints) != 0 {
						t.Fatal("racing hook waited for a poll tick or was lost")
					}
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if logs.Len() != 0 {
					t.Fatalf("expected waits produced logs: %s", logs.String())
				}
			})
		})
	}
}
