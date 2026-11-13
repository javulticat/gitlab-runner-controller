package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func protectedRetryFixture(t *testing.T, pool config.Pool) *fixture {
	t.Helper()
	f := setupLifecycle(t, provider.Disposable, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", pool.Name)}
	f.host.destroyVisible = true
	return f
}

func retryProbe(t *testing.T, f *fixture) state.PendingProbe {
	t.Helper()
	pool := f.cfg.Pools[0]
	probe, found := f.store.Snapshot().PendingProbes[state.PendingProbeKey(pool.RunnerID, 1, 1)]
	if !found {
		t.Fatal("pending probe was not saved")
	}
	return probe
}

func requirePausedProbe(t *testing.T, f *fixture, attempts int, remaining time.Duration) {
	t.Helper()
	probe := retryProbe(t, f)
	if probe.Exhausted || !probe.Deadline.IsZero() || probe.Remaining != remaining || probe.HostAttempts != attempts {
		t.Fatalf("paused probe = %+v, want %d hosts and %s remaining", probe, attempts, remaining)
	}
}

func TestProtectedProbeRetriesFailedBootstrapWithUnspentWindow(t *testing.T) {
	for _, checkpoint := range []string{"before-destroy", "after-destroy"} {
		t.Run(checkpoint, func(t *testing.T) {
			f := protectedRetryFixture(t, testPool("protected", 1, 1, 0))
			f.host.createState = provider.Stopped
			f.host.onCreate = func() {
				slot := f.store.Snapshot().Pools["protected"].Slots[0]
				if slot.Operation == nil || slot.Operation.ProbeKey != state.PendingProbeKey(f.cfg.Pools[0].RunnerID, 1, 1) || retryProbe(t, f).HostAttempts != f.host.creates-1 {
					t.Fatal("speculative create did not persist its probe reservation before dispatch")
				}
			}
			f.step(t, 0)
			first := f.store.Snapshot().Pools["protected"].Slots[0]
			requirePausedProbe(t, f, 1, f.cfg.ProbeTimeout())
			if checkpoint == "before-destroy" {
				f.restart(t)
			}
			f.now = first.BootstrapDeadline
			f.step(t, 0)
			slot := f.store.Snapshot().Pools["protected"].Slots[0]
			if f.host.destroys != 1 || f.host.creates != 1 || slot.ResourceID != first.ResourceID || slot.Operation == nil || slot.Operation.Kind != "destroy" {
				t.Fatalf("failed bootstrap did not await destruction confirmation: %+v", slot)
			}
			requirePausedProbe(t, f, 1, f.cfg.ProbeTimeout())
			if checkpoint == "after-destroy" {
				f.restart(t)
			}
			f.host.createState = provider.Running
			f.step(t, time.Duration(f.cfg.JobsPollInterval))
			second := f.store.Snapshot().Pools["protected"].Slots[0]
			if f.host.creates != 2 || second.ResourceID == "" || second.ResourceID == first.ResourceID {
				t.Fatalf("confirmed failed host was not replaced once: %+v", second)
			}
			requirePausedProbe(t, f, 2, f.cfg.ProbeTimeout())
			f.restart(t)
			f.step(t, 15*time.Second)
			probe := retryProbe(t, f)
			if !f.store.Snapshot().Pools["protected"].Slots[0].Ready || probe.ResourceID != second.ResourceID || !probe.Deadline.Equal(f.now.Add(f.cfg.ProbeTimeout())) || probe.Exhausted || probe.HostAttempts != 2 {
				t.Fatalf("replacement did not receive the full ready window: %+v", probe)
			}
			f.host.assertDisposableCalls(t)
		})
	}
}

func TestProtectedProbeFailedBootstrapsStopAtDurableHostLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{{"default-two", 0}, {"one", 1}, {"three", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool("protected", 1, 1, 0)
			pool.ProtectedProbeMaxHosts = tc.limit
			f := protectedRetryFixture(t, pool)
			f.host.createState = provider.Stopped
			f.step(t, 0)
			for attempt := 1; attempt <= pool.ProbeMaxHosts(); attempt++ {
				slot := f.store.Snapshot().Pools[pool.Name].Slots[0]
				f.now = slot.BootstrapDeadline
				f.step(t, 0)
				if f.host.creates != attempt || f.host.destroys != attempt {
					t.Fatalf("attempt %d: creates = %d, destroys = %d", attempt, f.host.creates, f.host.destroys)
				}
				f.restart(t)
				f.step(t, time.Duration(f.cfg.JobsPollInterval))
				if attempt < pool.ProbeMaxHosts() {
					requirePausedProbe(t, f, attempt+1, f.cfg.ProbeTimeout())
				}
			}
			probe := retryProbe(t, f)
			if !probe.Exhausted || probe.ExhaustionReason != "host_limit" || probe.HostAttempts != pool.ProbeMaxHosts() || probe.Remaining != f.cfg.ProbeTimeout() || !probe.Deadline.IsZero() || f.c.Status().Ready {
				t.Fatalf("host limit did not preserve the unspent window and readiness failure: %+v", probe)
			}
			for range 2 {
				f.restart(t)
				if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "pending"}); err != nil {
					t.Fatal(err)
				}
				f.step(t, time.Duration(f.cfg.JobsPollInterval))
			}
			f.cfg.Pools[0].MaxInstances = 2
			f.restart(t)
			f.host.resources["late-spare"] = provider.Resource{ID: "late-spare", State: provider.Running}
			if err := f.c.slotUpdate(pool.Name, 1, func(slot *state.Slot) {
				*slot = state.Slot{ResourceID: "late-spare", Power: provider.Running, Ready: true, ConfiguredJobs: 1}
			}); err != nil {
				t.Fatal(err)
			}
			f.step(t, 15*time.Second)
			if after := retryProbe(t, f); !after.Exhausted || after.ExhaustionReason != "host_limit" || !after.Deadline.IsZero() || after.Remaining != probe.Remaining || f.host.creates != pool.ProbeMaxHosts() || f.c.Status().Ready {
				t.Fatalf("restart, hook, or late spare revived the exhausted attempt: %+v", after)
			}
		})
	}
}

func TestProtectedProbeHostLossPreservesPoolOpportunity(t *testing.T) {
	for _, occupancy := range []string{"no-spare", "healthy-spare", "busy-spare", "unknown-busy-spare"} {
		t.Run(occupancy, func(t *testing.T) {
			pool := testPool("protected", 2, 1, 0)
			if occupancy == "no-spare" {
				pool.MaxInstances = 1
			}
			f := protectedRetryFixture(t, pool)
			f.step(t, 0)
			f.step(t, 15*time.Second)
			before := retryProbe(t, f)
			if occupancy != "no-spare" {
				f.host.resources["spare"] = provider.Resource{ID: "spare", State: provider.Running}
				if err := f.c.slotUpdate(pool.Name, 1, func(slot *state.Slot) {
					*slot = state.Slot{ResourceID: "spare", Power: provider.Running, Ready: true, ConfiguredJobs: 1}
				}); err != nil {
					t.Fatal(err)
				}
			}
			if occupancy == "busy-spare" || occupancy == "unknown-busy-spare" {
				busy := job(2, "running", pool.Name)
				if occupancy == "unknown-busy-spare" {
					busy.RunnerID = 0
				}
				f.api.jobs = append(f.api.jobs, busy)
			}
			f.host.resources[before.ResourceID] = provider.Resource{ID: before.ResourceID, State: provider.Stopped}
			f.step(t, time.Duration(f.cfg.JobsPollInterval))
			remaining := before.Deadline.Sub(f.now)
			if occupancy == "healthy-spare" {
				if probe := retryProbe(t, f); !probe.Deadline.Equal(before.Deadline) {
					t.Fatalf("another useful ready host shifted the opportunity deadline: %+v", probe)
				}
			} else {
				requirePausedProbe(t, f, 1, remaining)
			}
			f.restart(t)
			f.step(t, 15*time.Second)
			probe := retryProbe(t, f)
			if occupancy == "healthy-spare" {
				if !probe.Deadline.Equal(before.Deadline) || probe.ResourceID != "" || probe.HostAttempts != 1 || f.host.creates != 1 {
					t.Fatalf("destroy confirmation renewed or replaced an available opportunity: %+v", probe)
				}
			} else {
				if probe.Exhausted || !probe.Deadline.IsZero() || probe.Remaining != remaining {
					t.Fatalf("capacity loss spent or renewed the remaining opportunity: %+v", probe)
				}
			}
			if occupancy == "no-spare" {
				f.step(t, 15*time.Second)
				if probe := retryProbe(t, f); !probe.Deadline.Equal(f.now.Add(remaining)) || probe.HostAttempts != 2 {
					t.Fatalf("replacement reset the partially spent window: %+v", probe)
				}
			}
			if occupancy == "busy-spare" || occupancy == "unknown-busy-spare" {
				if f.host.resources["spare"].State != provider.Running || f.host.destroys != 1 || f.host.stops != 0 {
					t.Fatal("retry interrupted the host with assigned work")
				}
			}
		})
	}
}

func TestProtectedProbeHostBudgetChargesExtraHostsAsAssignedWorkGrows(t *testing.T) {
	pool := testPool("protected", 4, 1, 0)
	f := protectedRetryFixture(t, pool)
	f.step(t, 0)
	f.step(t, 15*time.Second)
	for _, id := range []int64{2, 3} {
		f.api.jobs = append(f.api.jobs, job(id, "running", pool.Name))
		if err := f.c.Hint(state.Hint{ProjectID: 1, ID: id, Status: "running"}); err != nil {
			t.Fatal(err)
		}
		f.step(t, time.Duration(f.cfg.MinRefresh))
		if id == 2 {
			if probe := retryProbe(t, f); probe.HostAttempts != 2 || probe.ResourceID != f.store.Snapshot().Pools[pool.Name].Slots[1].ResourceID {
				t.Fatalf("extra speculative host escaped accounting through the first busy host: %+v", probe)
			}
			f.step(t, 15*time.Second)
		}
	}
	probe := retryProbe(t, f)
	if f.host.creates != 2 || probe.HostAttempts != 2 || !probe.Exhausted || probe.ExhaustionReason != "host_limit" || f.host.stops != 0 || f.host.destroys != 0 || f.c.Status().Ready {
		t.Fatalf("growing assigned work bypassed the speculative host limit: %+v", probe)
	}
}

func TestProtectedProbeRejectedCreateDoesNotSpendHostBudget(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	pool.ProtectedProbeMaxHosts = 1
	f := protectedRetryFixture(t, pool)
	f.host.createErr = &provider.Error{Operation: "create", Kind: provider.Invalid, Detail: "rejected", NoSideEffect: true}
	f.step(t, 0)
	requirePausedProbe(t, f, 0, f.cfg.ProbeTimeout())
	f.restart(t)
	f.host.createErr = nil
	f.step(t, 31*time.Second)
	requirePausedProbe(t, f, 1, f.cfg.ProbeTimeout())
	f.step(t, 15*time.Second)
	if probe := retryProbe(t, f); probe.Exhausted || !probe.Deadline.Equal(f.now.Add(f.cfg.ProbeTimeout())) || f.host.creates != 2 {
		t.Fatalf("proven rejection consumed the single permitted host: %+v", probe)
	}
}

func TestProtectedProbeUncertainCreateWaitsForAuditedResolution(t *testing.T) {
	for _, found := range []bool{false, true} {
		name := "confirmed-absent"
		if found {
			name = "found-host"
		}
		t.Run(name, func(t *testing.T) {
			pool := testPool("protected", 1, 1, 0)
			pool.ProtectedProbeMaxHosts = 1
			f := protectedRetryFixture(t, pool)
			f.host.createErr = errors.New("create response was lost")
			f.step(t, 0)
			requirePausedProbe(t, f, 0, f.cfg.ProbeTimeout())
			slot := f.store.Snapshot().Pools[pool.Name].Slots[0]
			if slot.Operation == nil || slot.Operation.Kind != "unresolved" || slot.Operation.ProbeKey != state.PendingProbeKey(pool.RunnerID, 1, 1) || slot.ResourceID != "" {
				t.Fatalf("uncertain create lost its probe reservation: %+v", slot)
			}
			f.restart(t)
			f.host.createErr = nil
			if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "pending"}); err != nil {
				t.Fatal(err)
			}
			f.step(t, time.Duration(f.cfg.ReadinessTimeout))
			if f.host.creates != 1 {
				t.Fatal("restart or a hook replayed an uncertain create")
			}
			id := ""
			if found {
				id = "recovered-host"
				f.host.resources[id] = provider.Resource{ID: id, State: provider.Running}
			}
			if err := f.c.ResolveCreate(pool.Name, 0, id, !found); err != nil {
				t.Fatal(err)
			}
			if found {
				requirePausedProbe(t, f, 1, f.cfg.ProbeTimeout())
				if retryProbe(t, f).ResourceID != id {
					t.Fatal("audited found host was not attached to its pending probe")
				}
			} else {
				requirePausedProbe(t, f, 0, f.cfg.ProbeTimeout())
			}
			if err := f.c.ResolveCreate(pool.Name, 0, id, !found); err == nil {
				t.Fatal("the same create resolution was accepted twice")
			}
			f.restart(t)
			f.step(t, time.Duration(f.cfg.JobsPollInterval))
			if !found {
				f.step(t, 15*time.Second)
			}
			probe := retryProbe(t, f)
			wantCreates := 2
			if found {
				wantCreates = 1
			}
			if probe.HostAttempts != 1 || probe.Exhausted || !probe.Deadline.Equal(f.now.Add(f.cfg.ProbeTimeout())) || f.host.creates != wantCreates {
				t.Fatalf("audited resolution miscounted or replaced the host: %+v", probe)
			}
		})
	}
}

func TestProtectedProbeExistingReadyHostDoesNotSpendCreateBudget(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := protectedRetryFixture(t, pool)
	f.step(t, 0)
	probe := retryProbe(t, f)
	if probe.HostAttempts != 0 || probe.ResourceID != "owned" || !probe.Deadline.Equal(f.now.Add(f.cfg.ProbeTimeout())) || f.host.creates != 0 {
		t.Fatalf("existing ready capacity was charged as a speculative creation: %+v", probe)
	}
}

func TestProtectedProbeSpentOpportunitySurvivesAuditedMissingRelease(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	f := protectedRetryFixture(t, pool)
	f.step(t, 0)
	f.step(t, 15*time.Second)
	probe := retryProbe(t, f)
	f.now = probe.Deadline.Add(time.Second)
	f.step(t, 0)
	probe = retryProbe(t, f)
	if !probe.Exhausted || probe.ExhaustionReason != "opportunity_budget" {
		t.Fatalf("test did not spend the opportunity budget: %+v", probe)
	}
	id := f.store.Snapshot().Pools[pool.Name].Slots[0].ResourceID
	delete(f.host.resources, id)
	if _, err := ReleaseMissing(context.Background(), f.cfg, f.store, f.providers, pool.Name, 0, id, true); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	if after := retryProbe(t, f); !after.Exhausted || after.ExhaustionReason != "opportunity_budget" || after.Remaining != 0 || after.HostAttempts != probe.HostAttempts || f.host.creates != 1 {
		t.Fatalf("audited release renewed a spent opportunity: %+v", after)
	}
}
