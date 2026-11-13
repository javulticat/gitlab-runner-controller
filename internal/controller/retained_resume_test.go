package controller

import (
	"context"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type stalledResumeProvider struct {
	*fakeProvider
	resumeErr error
}

func (p *stalledResumeProvider) Resume(context.Context, string) error {
	p.resumes++
	return p.resumeErr
}

func stalledRetainedFixture(t *testing.T) (*fixture, *stalledResumeProvider) {
	t.Helper()
	pool := testPool("protected", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setup(t, pool)
	f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Stopped}
	host := &stalledResumeProvider{fakeProvider: f.host}
	f.providers["host"] = host
	f.api.jobs = []state.Job{job(1, "pending", pool.Name)}
	return f, host
}

func TestRetainedResumeDeadlineBoundsAcknowledgedRetriesAndPreservesProbeWindow(t *testing.T) {
	f, _ := stalledRetainedFixture(t)
	pool := f.cfg.Pools[0]
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.step(t, 0)
	initial := f.store.Snapshot().Pools[pool.Name].Slots[0]
	if initial.Operation == nil || initial.Operation.Kind != "resume" || initial.BootstrapDeadline.IsZero() {
		t.Fatalf("resume did not save its readiness interval: %+v", initial)
	}
	f.step(t, time.Duration(f.cfg.ObservationGrace)-time.Second)
	if f.host.resumes != 1 {
		t.Fatal("resume was repeated before the observation grace elapsed")
	}
	f.step(t, time.Second)
	if f.host.resumes != 2 {
		t.Fatal("resume was not retried after the observation grace elapsed")
	}
	for f.now.Before(initial.BootstrapStartedAt.Add(30 * time.Minute)) {
		before := f.store.Snapshot().Pools[pool.Name].Slots[0]
		calls := f.host.resumes
		f.step(t, 15*time.Second)
		slot := f.store.Snapshot().Pools[pool.Name].Slots[0]
		if !slot.BootstrapStartedAt.Equal(initial.BootstrapStartedAt) || !slot.BootstrapDeadline.Equal(initial.BootstrapDeadline) {
			t.Fatal("an acknowledgment renewed the bootstrap interval")
		}
		if !f.now.Before(initial.BootstrapDeadline) {
			if slot.LastError == "" || !slot.RetryAt.After(f.now) || f.c.Status().Ready {
				t.Fatalf("expired resume lost its visible failure and backoff: %+v", slot)
			}
			if before.LastError != "" && f.now.Before(before.RetryAt) && (!slot.RetryAt.Equal(before.RetryAt) || f.host.resumes != calls) {
				t.Fatal("observation moved the retry timer or dispatched during backoff")
			}
		}
		if f.now.Sub(initial.BootstrapStartedAt) == 12*time.Minute {
			f.restart(t)
		}
	}
	if f.host.resumes >= 15 || f.host.creates != 0 || f.host.stops != 0 || f.host.destroys != 0 {
		t.Fatalf("retained recovery repeated too often or changed lifecycle: resume=%d create=%d stop=%d destroy=%d", f.host.resumes, f.host.creates, f.host.stops, f.host.destroys)
	}
	probe := f.store.Snapshot().PendingProbes[state.PendingProbeKey(pool.RunnerID, 1, 1)]
	if probe.Exhausted || !probe.Deadline.IsZero() || probe.Remaining != f.cfg.ProbeTimeout() {
		t.Fatalf("resume acknowledgments consumed a protected job's useful window: %+v", probe)
	}
	f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Running}
	f.host.configurePending = true
	f.step(t, 15*time.Second)
	slot := f.store.Snapshot().Pools[pool.Name].Slots[0]
	if slot.Operation != nil || slot.Power != provider.Running || !slot.Persisted || slot.Ready || slot.LastError == "" || !slot.BootstrapDeadline.Equal(initial.BootstrapDeadline) || f.host.configures != 1 || f.c.Status().Ready {
		t.Fatalf("running observation or persistence acknowledgment concealed an unready runner: %+v", slot)
	}
	f.host.configurePending = false
	f.now = slot.RetryAt
	f.step(t, 0)
	slot = f.store.Snapshot().Pools[pool.Name].Slots[0]
	if !slot.Ready || slot.LastError != "" || !slot.RetryAt.IsZero() || !slot.BootstrapDeadline.IsZero() || !f.c.Status().Ready {
		t.Fatalf("confirmed readiness did not finish retained recovery: %+v", slot)
	}
	probe = f.store.Snapshot().PendingProbes[state.PendingProbeKey(pool.RunnerID, 1, 1)]
	if probe.Exhausted || !probe.Deadline.Equal(f.now.Add(f.cfg.ProbeTimeout())) {
		t.Fatalf("ready runner did not receive the original useful window: %+v", probe)
	}
}

func TestRetainedCreationTimeoutKeepsIdentityAndOriginalResumeDeadline(t *testing.T) {
	pool := testPool("linux", 1, 1, 0)
	f := setup(t, pool)
	f.host.createState = provider.Stopped
	f.providers["host"] = &stalledResumeProvider{fakeProvider: f.host}
	f.api.jobs = []state.Job{job(1, "pending", pool.Name)}
	f.step(t, 0)
	initial := f.store.Snapshot().Pools[pool.Name].Slots[0]
	if initial.Operation == nil || initial.Operation.Kind != "await-running" {
		t.Fatal("creation did not await running inventory")
	}
	f.now = initial.BootstrapDeadline
	f.step(t, 0)
	slot := f.store.Snapshot().Pools[pool.Name].Slots[0]
	if slot.ResourceID != initial.ResourceID || slot.Operation == nil || slot.Operation.Kind != "resume" || slot.Operation.ID != initial.Operation.ID || !slot.BootstrapStartedAt.Equal(initial.BootstrapStartedAt) || !slot.BootstrapDeadline.Equal(initial.BootstrapDeadline) || slot.LastError == "" || !slot.RetryAt.After(f.now) || f.c.Status().Ready {
		t.Fatalf("retained creation timeout discarded identity or renewed its deadline: %+v", slot)
	}
	f.restart(t)
	f.step(t, 15*time.Second)
	if after := f.store.Snapshot().Pools[pool.Name].Slots[0]; !after.RetryAt.Equal(slot.RetryAt) || !after.BootstrapDeadline.Equal(initial.BootstrapDeadline) || f.host.resumes != 0 {
		t.Fatal("restart moved the retained creation recovery timer")
	}
	f.now = slot.RetryAt
	f.step(t, 0)
	slot = f.store.Snapshot().Pools[pool.Name].Slots[0]
	if f.host.creates != 1 || f.host.resumes != 1 || f.host.destroys != 0 || f.host.stops != 0 || slot.ResourceID != initial.ResourceID || !slot.BootstrapDeadline.Equal(initial.BootstrapDeadline) || slot.LastError == "" || !slot.RetryAt.After(f.now) || f.c.Status().Ready {
		t.Fatalf("retained recovery did not preserve bounded retries of the saved host: %+v", slot)
	}
}

func TestRetainedResumeErrorsKeepDeadlineAcrossRejectedRequestsAndRestart(t *testing.T) {
	for _, noSideEffect := range []bool{false, true} {
		name := "uncertain-effect"
		if noSideEffect {
			name = "rejected-before-dispatch"
		}
		t.Run(name, func(t *testing.T) {
			f, host := stalledRetainedFixture(t)
			host.resumeErr = &provider.Error{Operation: "resume", Kind: provider.Transient, Detail: "temporary resume failure", NoSideEffect: noSideEffect}
			f.step(t, 0)
			initial := f.store.Snapshot().Pools["protected"].Slots[0]
			f.now = initial.BootstrapDeadline
			for range 3 {
				f.step(t, 0)
				slot := f.store.Snapshot().Pools["protected"].Slots[0]
				if !slot.BootstrapStartedAt.Equal(initial.BootstrapStartedAt) || !slot.BootstrapDeadline.Equal(initial.BootstrapDeadline) || slot.LastError == "" || slot.RetryAt.Before(f.now.Add(time.Duration(f.cfg.ReadinessTimeout))) || f.c.Status().Ready {
					t.Fatalf("resume error renewed the interval or removed timeout backoff: %+v", slot)
				}
				calls := f.host.resumes
				f.restart(t)
				f.step(t, 15*time.Second)
				if after := f.store.Snapshot().Pools["protected"].Slots[0]; !after.RetryAt.Equal(slot.RetryAt) || f.host.resumes != calls {
					t.Fatal("restart retried a failed resume during backoff")
				}
				f.now = slot.RetryAt
			}
			host.resumeErr = nil
			f.step(t, 0)
			slot := f.store.Snapshot().Pools["protected"].Slots[0]
			if slot.LastError == "" || !slot.BootstrapDeadline.Equal(initial.BootstrapDeadline) || !slot.RetryAt.After(f.now) || f.c.Status().Ready {
				t.Fatal("a later acknowledgment cleared the failed resume without readiness")
			}
			f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Running}
			f.step(t, 15*time.Second)
			if after := f.store.Snapshot().Pools["protected"].Slots[0]; !after.Ready || after.LastError != "" || !after.RetryAt.IsZero() || !f.c.Status().Ready {
				t.Fatalf("a running and configured runner did not recover after resume errors: %+v", after)
			}
		})
	}
}

func TestStatusRejectsExpiredStoppedResumeBeforeReconciliation(t *testing.T) {
	f, _ := stalledRetainedFixture(t)
	f.step(t, 0)
	deadline := f.store.Snapshot().Pools["protected"].Slots[0].BootstrapDeadline
	f.now = deadline.Add(-time.Second)
	f.step(t, 0)
	if !f.c.Status().Ready {
		t.Fatal("resume changed service readiness before the existing observation interval expired")
	}
	f.now = deadline
	if f.store.Snapshot().Pools["protected"].Slots[0].LastError != "" || f.c.Status().Ready {
		t.Fatal("status accepted an expired stopped resume before its next reconciliation")
	}
}

func TestRetainedResumeErrorNearDeadlineStartsOneRecoveryBackoff(t *testing.T) {
	for _, noSideEffect := range []bool{false, true} {
		name := "uncertain-effect"
		if noSideEffect {
			name = "rejected-before-dispatch"
		}
		t.Run(name, func(t *testing.T) {
			f, host := stalledRetainedFixture(t)
			host.resumeErr = &provider.Error{Operation: "resume", Kind: provider.Transient, Detail: "temporary resume failure", NoSideEffect: noSideEffect}
			f.step(t, 0)
			deadline := f.store.Snapshot().Pools["protected"].Slots[0].BootstrapDeadline
			f.now = deadline.Add(-time.Second)
			f.step(t, 0)
			before := f.store.Snapshot().Pools["protected"].Slots[0]
			calls := f.host.resumes
			host.resumeErr = nil
			f.now = deadline
			f.step(t, 0)
			slot := f.store.Snapshot().Pools["protected"].Slots[0]
			if slot.LastError != before.LastError || !slot.BootstrapDeadline.Equal(deadline) || !slot.RetryAt.Equal(f.now.Add(time.Duration(f.cfg.ReadinessTimeout))) || f.host.resumes != calls || f.c.Status().Ready {
				t.Fatalf("preexisting error prevented the first timeout backoff: %+v", slot)
			}
			f.restart(t)
			f.step(t, 15*time.Second)
			if after := f.store.Snapshot().Pools["protected"].Slots[0]; !after.RetryAt.Equal(slot.RetryAt) || f.host.resumes != calls {
				t.Fatal("restart or observation moved the first timeout backoff")
			}
			f.now = slot.RetryAt
			f.step(t, 0)
			if after := f.store.Snapshot().Pools["protected"].Slots[0]; !after.BootstrapDeadline.Equal(deadline) || after.LastError != before.LastError || !after.RetryAt.After(f.now) || f.host.resumes != calls+1 || f.c.Status().Ready {
				t.Fatalf("successful retry acknowledgment concealed an expired runner: %+v", after)
			}
		})
	}
}

func TestRejectedRetainedResumeClearsUnusedIntervalAfterDemandDisappears(t *testing.T) {
	f, host := stalledRetainedFixture(t)
	host.resumeErr = &provider.Error{Operation: "resume", Kind: provider.Transient, Detail: "resume was rejected", NoSideEffect: true}
	f.step(t, 0)
	initial := f.store.Snapshot().Pools["protected"].Slots[0]
	if initial.Operation != nil || initial.BootstrapDeadline.IsZero() || initial.LastError == "" {
		t.Fatal("rejected resume did not retain the failed activation interval")
	}
	f.api.jobs = nil
	f.restart(t)
	f.now = initial.BootstrapDeadline.Add(time.Second)
	f.step(t, 0)
	slot := f.store.Snapshot().Pools["protected"].Slots[0]
	if slot.ResourceID != initial.ResourceID || slot.Persisted != initial.Persisted || slot.Power != provider.Stopped || slot.Operation != nil || !slot.BootstrapStartedAt.IsZero() || !slot.BootstrapDeadline.IsZero() || slot.LastError != "" || !slot.RetryAt.IsZero() || !f.c.Status().Ready {
		t.Fatalf("verified idle demand left a rejected activation permanently unready: %+v", slot)
	}
	if f.host.resumes != 1 || f.host.stops != 0 || f.host.destroys != 0 {
		t.Fatal("canceling unused readiness tracking mutated the saved host")
	}
	f.api.jobs = []state.Job{job(2, "pending", "protected")}
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	slot = f.store.Snapshot().Pools["protected"].Slots[0]
	if f.host.resumes != 2 || !slot.BootstrapDeadline.Equal(f.now.Add(time.Duration(f.cfg.ReadinessTimeout))) || slot.ResourceID != initial.ResourceID {
		t.Fatalf("later verified demand did not start a fresh interval for the saved host: %+v", slot)
	}
}

func TestRetainedResumeTimeoutAllowsIndependentProviderScheduling(t *testing.T) {
	f, _ := stalledRetainedFixture(t)
	other := testPool("other", 1, 1, 0)
	other.Provider = "independent"
	f.cfg.Pools = append(f.cfg.Pools, other)
	f.cfg.Providers[other.Provider] = testProvider(provider.Retained, 5)
	otherSpec := f.cfg.Providers[other.Provider]
	otherSpec.Identity = "account-independent"
	f.cfg.Providers[other.Provider] = otherSpec
	otherHost := newFakeProvider(provider.Retained)
	otherHost.description.Identity = otherSpec.Identity
	f.providers[other.Provider] = otherHost
	f.restart(t)
	f.step(t, 0)
	f.now = f.store.Snapshot().Pools["protected"].Slots[0].BootstrapDeadline
	f.step(t, 0)
	f.api.jobs = append(f.api.jobs, job(2, "pending", other.Name))
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	f.step(t, 15*time.Second)
	if otherHost.creates != 1 || !f.store.Snapshot().Pools[other.Name].Slots[0].Ready || f.c.Status().Ready {
		t.Fatal("one retained resume timeout blocked an independent provider or hid degraded readiness")
	}
}
