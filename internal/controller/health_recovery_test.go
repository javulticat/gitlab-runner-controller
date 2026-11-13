package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type healthRecoveryProvider struct {
	*fakeProvider
	healthErr error
}

func (p *healthRecoveryProvider) Healthy(context.Context, string, json.RawMessage, string) (bool, error) {
	return p.healthErr == nil, p.healthErr
}

func TestRecoveredHealthClearsErrorAndResumesPendingProbeBudget(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", "protected")}
	host := &healthRecoveryProvider{fakeProvider: f.host}
	f.providers["host"] = host
	f.step(t, 0)
	host.healthErr = &provider.Error{Operation: "healthy", Kind: provider.Transient, Detail: "temporary inspection failure"}
	f.step(t, 15*time.Second)
	if f.store.Snapshot().Pools[pool.Name].Slots[0].LastError == "" {
		t.Fatal("health failure was hidden")
	}
	host.healthErr = nil
	f.step(t, 31*time.Second)
	slot := f.store.Snapshot().Pools[pool.Name].Slots[0]
	if slot.LastError != "" || !slot.RetryAt.IsZero() || !f.c.Status().Ready {
		t.Fatal("recovered health retained stale error or readiness failure")
	}
	key := state.PendingProbeKey(pool.RunnerID, 1, 1)
	if f.store.Snapshot().PendingProbes[key].Deadline.IsZero() {
		t.Fatal("healthy capacity did not resume the bounded opportunity")
	}
	f.step(t, 2*time.Minute)
	if !f.store.Snapshot().PendingProbes[key].Exhausted {
		t.Fatal("recovered host permanently retained the unknown job")
	}
	f.step(t, time.Duration(f.cfg.IdleGrace))
	if f.host.stops != 1 {
		t.Fatal("exhausted protected job prevented idle release after health recovery")
	}
}

func TestUnknownBusyAssignmentDoesNotConsumeProtectedProbeOpportunity(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	busy := job(1, "running", "protected")
	busy.RunnerID = 0
	f.api.jobs = []state.Job{busy, job(2, "pending", "protected")}
	f.step(t, 0)
	f.step(t, 3*time.Minute)
	for _, probe := range f.store.Snapshot().PendingProbes {
		if probe.Exhausted || !probe.Deadline.IsZero() {
			t.Fatal("unknown occupancy consumed a probe without verified spare capacity")
		}
	}
	f.api.jobs = []state.Job{job(2, "pending", "protected")}
	f.step(t, time.Minute)
	probe, exists := f.store.Snapshot().PendingProbes[state.PendingProbeKey(pool.RunnerID, 1, 2)]
	if !exists || probe.Deadline.IsZero() || probe.Exhausted {
		t.Fatal("probe did not begin after spare ready capacity became known")
	}
}
