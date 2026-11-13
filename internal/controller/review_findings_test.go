package controller

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/gitlab"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type jobsWithoutPolicies struct{ jobs *fakeJobs }

func (api jobsWithoutPolicies) Sync(ctx context.Context, projects []int64, previous []state.Job) ([]state.Job, error) {
	return api.jobs.Sync(ctx, projects, previous)
}

func TestProviderFailureDoesNotBlockIndependentAccount(t *testing.T) {
	for _, release := range []bool{false, true} {
		name := "allocate"
		if release {
			name = "release"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t, testPool("blocked", 1, 1, 0))
			otherPool := testPool("available", 1, 1, 0)
			otherPool.Provider = "other"
			other := newFakeProvider(provider.Retained)
			other.description.Identity = "account-other"
			otherSpec := testProvider(provider.Retained, 1)
			otherSpec.Identity = other.description.Identity
			f.cfg.Providers["other"] = otherSpec
			f.providers["other"] = other
			if release {
				otherPool.AdoptIDs = []string{"owned"}
				other.resources["owned"] = provider.Resource{ID: "owned", State: provider.Running}
			} else {
				f.api.jobs = []state.Job{job(1, "pending", "available")}
			}
			f.cfg.Pools = append(f.cfg.Pools, otherPool)
			f.construct(t)
			f.host.listErr = &provider.Error{Operation: "list", Kind: provider.Transient, RetryAfter: 2 * time.Minute}
			if err := f.c.Step(context.Background()); err == nil {
				t.Fatal("provider failure was not reported")
			}
			if !release && other.creates != 1 {
				t.Fatal("failed account blocked allocation in the independent account")
			}
			lists := f.host.lists
			f.now = f.now.Add(time.Minute)
			if err := f.c.Step(context.Background()); err == nil {
				t.Fatal("provider backoff was not reported")
			}
			if f.host.lists != lists || f.host.mutations() != 0 {
				t.Fatal("failed account was called during its backoff")
			}
			if release && other.stops != 1 {
				t.Fatal("account backoff blocked idle release in the independent account")
			}
			if !release && other.configures != 1 {
				t.Fatal("account backoff blocked bootstrap in the independent account")
			}
		})
	}
}

func TestRunnerAssignmentControlsBusyDemand(t *testing.T) {
	for _, owned := range []bool{false, true} {
		name := "foreign-runner-with-matching-tags"
		if owned {
			name = "owned-runner-with-changed-tags"
		}
		t.Run(name, func(t *testing.T) {
			pool := testPool("linux", 2, 1, 0)
			pool.AdoptIDs = []string{"owned"}
			f := setup(t, pool)
			busy := job(1, "running", "linux")
			if owned {
				busy.Tags = []string{"changed-after-assignment"}
			} else {
				busy.RunnerID = poolRunnerID("foreign")
			}
			f.api.jobs = []state.Job{busy, job(2, "pending", "linux")}
			f.step(t, 0)
			want := 0
			if owned {
				want = 1
			}
			if f.host.creates != want {
				t.Fatalf("created %d resources, want %d based on actual runner assignment", f.host.creates, want)
			}
			f.api.jobs = []state.Job{busy}
			f.step(t, time.Minute)
			f.step(t, time.Minute)
			if owned && f.host.stops != 0 {
				t.Fatal("assigned work was stopped after its tags changed")
			}
			if !owned && f.host.stops != 1 {
				t.Fatal("another runner's work held idle capacity")
			}
		})
	}
}

func TestUnknownRunnerAssignmentPreservesCapacityWithoutScaleOut(t *testing.T) {
	pool := testPool("linux", 3, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setup(t, pool)
	busy := job(1, "running", "linux")
	busy.RunnerID = 0
	f.api.jobs = []state.Job{busy}
	f.step(t, 0)
	f.step(t, 2*time.Minute)
	if f.host.stops != 0 || f.host.creates != 0 || f.host.resumes != 0 {
		t.Fatal("unknown assignment released existing capacity or allocated new capacity")
	}
}

func TestPendingDemandRequiresVerifiedRunnerEligibility(t *testing.T) {
	cases := []struct {
		name   string
		change func(*gitlab.RunnerPolicy)
	}{
		{"unknown-runner", func(policy *gitlab.RunnerPolicy) { policy.Known = false }},
		{"paused-runner", func(policy *gitlab.RunnerPolicy) { policy.Paused = true }},
		{"different-actual-tags", func(policy *gitlab.RunnerPolicy) { policy.Tags = []string{"other"} }},
		{"unavailable-to-project", func(policy *gitlab.RunnerPolicy) { policy.Projects[1] = false }},
		{"unknown-project-availability", func(policy *gitlab.RunnerPolicy) { delete(policy.Projects, 1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool("linux", 3, 1, 0)
			f := setup(t, pool)
			policy := f.api.policies[pool.RunnerID]
			tc.change(&policy)
			f.api.policies[pool.RunnerID] = policy
			f.api.jobs = []state.Job{job(1, "pending", "linux")}
			if err := f.c.Step(context.Background()); errors.Is(err, state.ErrPoisoned) {
				t.Fatal(err)
			}
			if f.host.mutations() != 0 {
				t.Fatal("unverified or incompatible runner policy allocated capacity")
			}
		})
	}
}

func TestMissingRunnerPolicyAPIStopsPendingAllocation(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	c, err := New(f.cfg, f.store, jobsWithoutPolicies{f.api}, f.providers, f.c.log)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return f.now }
	if err := c.Step(context.Background()); errors.Is(err, state.ErrPoisoned) {
		t.Fatal(err)
	}
	if f.host.mutations() != 0 || c.Status().Ready {
		t.Fatal("missing policy API allowed allocation or reported verified readiness")
	}
}

func TestRunnerPolicyRateLimitBlocksLifecycleWork(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.api.policiesErr = &gitlab.RateLimitError{Delay: 2 * time.Minute}
	if err := f.c.Step(context.Background()); err == nil {
		t.Fatal("runner policy rate limit was ignored")
	}
	if !f.store.Snapshot().BackoffUntil.Equal(f.now.Add(2*time.Minute)) || f.host.lists != 0 || f.host.mutations() != 0 {
		t.Fatal("policy rate limit allowed lifecycle work or failed to persist GitLab backoff")
	}
}

func TestProtectedPendingProbeIsBoundedAndDurable(t *testing.T) {
	pool := testPool("protected", 4, 1, 0)
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	for id := range int64(10) {
		f.api.jobs = append(f.api.jobs, job(id+1, "pending", "protected"))
	}
	f.host.onCreate = func() {
		probes := f.store.Snapshot().PendingProbes
		if len(probes) != 1 {
			t.Error("probe creation lacked a durable reservation")
		}
		for _, probe := range probes {
			if probe.Pool != pool.Name || probe.Slot != 0 || probe.ReservedAt.IsZero() || !probe.Deadline.IsZero() {
				t.Error("probe reservation did not identify the selected slot before activation")
			}
		}
	}
	f.step(t, 0)
	if f.host.creates != 1 || len(f.store.Snapshot().PendingProbes) != 1 {
		t.Fatal("unknown protection eligibility allocated more than one probe")
	}
	f.step(t, 15*time.Second)
	var key string
	var first state.PendingProbe
	for key, first = range f.store.Snapshot().PendingProbes {
	}
	if first.Deadline.IsZero() || first.Exhausted {
		t.Fatal("ready idle probe did not begin its bounded observation window")
	}
	f.api.jobs = append(f.api.jobs, job(99, "pending", "protected"))
	if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 99, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	f.step(t, time.Duration(f.cfg.MinRefresh))
	if probes := f.store.Snapshot().PendingProbes; len(probes) != 1 || !probes[key].Deadline.Equal(first.Deadline) {
		t.Fatal("a new unknown job extended or duplicated the active probe")
	}
	f.api.jobs = []state.Job{job(first.JobID, "pending", "protected")}
	f.restart(t)
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	if got := f.store.Snapshot().PendingProbes[key]; !got.Deadline.Equal(first.Deadline) || got.Exhausted {
		t.Fatal("restart changed an active probe's deadline")
	}
	f.now = first.Deadline.Add(time.Second)
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	if !f.store.Snapshot().PendingProbes[key].Exhausted {
		t.Fatal("a complete later job snapshot did not exhaust the unassigned probe")
	}
	f.step(t, 2*time.Minute)
	if f.host.stops != 1 {
		t.Fatal("exhausted protection probes kept idle capacity indefinitely")
	}
	f.restart(t)
	if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 999, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	f.step(t, 2*time.Minute)
	if f.host.creates != 1 || f.host.resumes != 0 || !f.store.Snapshot().PendingProbes[key].Exhausted {
		t.Fatal("restart or an unrelated hook repeated an exhausted probe")
	}
}

func TestProtectedProbeWindowStartsAfterRunnerReadiness(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.host.configurePending = true
	f.api.jobs = []state.Job{job(1, "pending", "protected")}
	f.step(t, 0)
	f.step(t, f.cfg.ProbeTimeout()+time.Second)
	key := state.PendingProbeKey(pool.RunnerID, 1, 1)
	probe, exists := f.store.Snapshot().PendingProbes[key]
	if !exists || !probe.Deadline.IsZero() || probe.Exhausted {
		t.Fatal("bootstrap time consumed the pending probe window")
	}
	f.host.configurePending = false
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	probe = f.store.Snapshot().PendingProbes[key]
	if !probe.Deadline.Equal(f.now.Add(f.cfg.ProbeTimeout())) || probe.Exhausted {
		t.Fatal("ready idle runner did not receive its complete probe window")
	}
}

func TestProtectedProbeDeadlineRequiresUsefulIdleCapacity(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "running", "protected"), job(2, "pending", "protected")}
	f.step(t, 0)
	f.step(t, 2*time.Minute)
	for _, probe := range f.store.Snapshot().PendingProbes {
		if !probe.Deadline.IsZero() || probe.Exhausted {
			t.Fatal("busy pool consumed a probe observation window")
		}
	}
	if f.host.stops != 0 {
		t.Fatal("probe handling interrupted assigned work")
	}
}

func TestAssignedProbeCompletesBeforeTheNextPendingAttempt(t *testing.T) {
	pool := testPool("protected", 2, 1, 0)
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", "protected")}
	f.step(t, 0)
	f.step(t, 15*time.Second)
	f.api.jobs = []state.Job{job(1, "running", "protected"), job(2, "pending", "protected")}
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	saved := f.store.Snapshot()
	first, firstExists := saved.PendingProbes[state.PendingProbeKey(pool.RunnerID, 1, 1)]
	second, exists := saved.PendingProbes[state.PendingProbeKey(pool.RunnerID, 1, 2)]
	if firstExists && !first.Exhausted || !exists || second.Exhausted || f.host.creates != 2 {
		t.Fatal("assigned probe kept the next pending job from receiving its own bounded attempt")
	}
	if f.host.stops != 0 || f.host.destroys != 0 {
		t.Fatal("completing a probe interrupted assigned work")
	}
}

func TestStrictProtectedPendingPolicyDoesNotProbe(t *testing.T) {
	pool := testPool("protected", 2, 1, 0)
	pool.ProtectedPendingPolicy = "strict"
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", "protected")}
	f.step(t, 0)
	if f.host.creates != 0 || len(f.store.Snapshot().PendingProbes) != 0 {
		t.Fatal("strict protection policy started a speculative runner")
	}
}

func TestBootstrapDeadlineSurvivesRestartAndPreservesBusyHost(t *testing.T) {
	pool := testPool("linux", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setup(t, pool)
	f.host.configurePending = true
	f.api.jobs = []state.Job{job(1, "running", "linux")}
	f.step(t, 0)
	before := f.store.Snapshot().Pools[pool.Name].Slots[0]
	if before.Ready || before.BootstrapStartedAt.IsZero() || before.BootstrapDeadline.IsZero() || f.c.Status().Ready {
		t.Fatal("running resource was treated as a ready runner before bootstrap finished")
	}
	f.step(t, time.Duration(f.cfg.ReadinessTimeout)/2)
	f.restart(t)
	f.now = before.BootstrapDeadline.Add(time.Second)
	f.step(t, 0)
	after := f.store.Snapshot().Pools[pool.Name].Slots[0]
	if !after.BootstrapDeadline.Equal(before.BootstrapDeadline) || after.Ready || after.LastError == "" || f.c.Status().Ready {
		t.Fatal("bootstrap deadline restarted or failed to report a stuck runner")
	}
	if f.host.stops != 0 || f.host.destroys != 0 || after.ResourceID != "owned" {
		t.Fatal("bootstrap timeout interrupted a host with assigned work")
	}
}

func TestRemovedDisposablePoolCannotForgetStoppedResource(t *testing.T) {
	pool := testPool("linux", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setupLifecycle(t, provider.Disposable, pool)
	f.api.jobs = []state.Job{job(1, "running", "linux")}
	f.step(t, 0)
	if err := f.c.slotUpdate(pool.Name, 0, func(slot *state.Slot) {
		slot.Power = provider.Stopped
		slot.Ready = false
	}); err != nil {
		t.Fatal(err)
	}
	f.cfg.Pools = []config.Pool{testPool("replacement", 1, 1, 0)}
	if _, err := New(f.cfg, f.store, f.api, f.providers, f.c.log); err == nil {
		t.Fatal("pool removal forgot a stopped but existing disposable resource")
	}
	if f.store.Snapshot().Pools[pool.Name].Slots[0].ResourceID != "owned" {
		t.Fatal("rejected removal changed ownership")
	}
}

func TestRunnerBindingCannotChangeWhileSavedHostsExist(t *testing.T) {
	pool := testPool("linux", 1, 1, 0)
	pool.AdoptIDs = []string{"owned"}
	f := setup(t, pool)
	f.api.jobs = []state.Job{job(1, "running", "linux")}
	f.step(t, 0)
	f.cfg.Pools[0].RunnerID = poolRunnerID("replacement")
	if _, err := New(f.cfg, f.store, f.api, f.providers, f.c.log); err == nil {
		t.Fatal("runner identity changed while saved hosts retained the previous runner configuration")
	}
	if f.store.Snapshot().Pools[pool.Name].RunnerID != pool.RunnerID {
		t.Fatal("rejected runner binding change changed durable ownership")
	}
}

func TestUnusedRemovedProviderBindingCanBeRenamed(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.step(t, 0)
	if len(f.store.Snapshot().ProviderBindings) != 1 {
		t.Fatal("provider binding was not established")
	}
	delete(f.cfg.Providers, "host")
	f.cfg.Providers["renamed"] = testProvider(provider.Retained, 5)
	f.cfg.Pools[0].Provider = "renamed"
	f.providers = map[string]provider.Provider{"renamed": f.host}
	f.construct(t)
	f.step(t, time.Minute)
	bindings := f.store.Snapshot().ProviderBindings
	if _, old := bindings["host"]; old || len(bindings) != 1 || bindings["renamed"].Identity != "account-test" {
		t.Fatal("unused removed binding prevented an account alias rename")
	}
}

func TestUnchangedReconciliationDoesNotWriteState(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.step(t, 0)
	before := f.store.Snapshot()
	calls := f.api.calls
	f.step(t, time.Second)
	after := f.store.Snapshot()
	if f.api.calls != calls || after.Revision != before.Revision {
		t.Fatalf("unchanged cycle refreshed jobs or rewrote state: API %d -> %d, revision %d -> %d", calls, f.api.calls, before.Revision, after.Revision)
	}
}

func TestReleaseMissingAuditsBacksUpAndPermitsReplacement(t *testing.T) {
	for _, lifecycle := range []provider.Lifecycle{provider.Retained, provider.Disposable} {
		t.Run(string(lifecycle), func(t *testing.T) {
			pool := testPool("linux", 1, 1, 0)
			pool.AdoptIDs = []string{"owned"}
			f := setupLifecycle(t, lifecycle, pool)
			f.api.jobs = []state.Job{job(1, "pending", "linux")}
			f.step(t, 0)
			delete(f.host.resources, "owned")
			f.step(t, 15*time.Second)
			before, err := os.ReadFile(f.path)
			if err != nil {
				t.Fatal(err)
			}
			mutations, describes, lists := f.host.mutations(), f.host.describes, f.host.lists
			backup, err := ReleaseMissing(context.Background(), f.cfg, f.store, f.providers, pool.Name, 0, "owned", true)
			if err != nil {
				t.Fatal(err)
			}
			copied, err := os.ReadFile(backup)
			if err != nil || string(copied) != string(before) {
				t.Fatal("recovery backup did not preserve the exact prior snapshot")
			}
			info, err := os.Stat(backup)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("recovery backup was not private")
			}
			if f.host.mutations() != mutations || f.host.describes != describes+1 || f.host.lists != lists+1 {
				t.Fatal("missing resource recovery skipped fresh verification or changed provider resources")
			}
			saved := f.store.Snapshot()
			slot := saved.Pools[pool.Name].Slots[0]
			if slot.ResourceID != "" || slot.Operation != nil || slot.Power != provider.Stopped || len(saved.Audit) != 1 {
				t.Fatal("confirmed absence did not release ownership with a durable audit record")
			}
			audit := saved.Audit[0]
			if audit.ID == "" || audit.At.IsZero() || audit.Action != "release-missing" || audit.Pool != pool.Name || audit.Slot != 0 || audit.ResourceID != "owned" || audit.Binding != saved.ProviderBindings["host"] {
				t.Fatal("missing resource audit omitted the prior ownership identity")
			}
			f.restart(t)
			f.step(t, time.Minute)
			if f.host.creates != 1 || f.store.Snapshot().Pools[pool.Name].Slots[0].ResourceID == "owned" {
				t.Fatal("audited release did not permit a fresh replacement after restart")
			}
		})
	}
}

func TestReleaseMissingRejectsUnverifiedAbsenceAndUnresolvedCreates(t *testing.T) {
	for _, reason := range []string{"present-running", "present-stopped", "foreign-account", "foreign-scope", "failed-inventory", "wrong-id", "no-confirmation", "unresolved-create"} {
		t.Run(reason, func(t *testing.T) {
			pool := testPool("linux", 1, 1, 0)
			pool.AdoptIDs = []string{"owned"}
			f := setup(t, pool)
			f.api.jobs = []state.Job{job(1, "running", "linux")}
			f.step(t, 0)
			delete(f.host.resources, "owned")
			f.step(t, 15*time.Second)
			expected, confirm := "owned", true
			switch reason {
			case "present-running":
				f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Running}
			case "present-stopped":
				f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Stopped}
			case "foreign-account":
				f.host.description.Identity = "another-account"
			case "foreign-scope":
				f.host.description.Scope = "another-scope"
			case "failed-inventory":
				f.host.listErr = errors.New("inventory unavailable")
			case "wrong-id":
				expected = "different-id"
			case "no-confirmation":
				confirm = false
			case "unresolved-create":
				if err := f.c.slotUpdate(pool.Name, 0, func(slot *state.Slot) {
					*slot = state.Slot{Power: provider.Stopped, Operation: &state.Operation{Kind: "unresolved", ID: "lost-create", StartedAt: f.now}}
				}); err != nil {
					t.Fatal(err)
				}
				expected = ""
			}
			before := f.store.Snapshot()
			mutations := f.host.mutations()
			if _, err := ReleaseMissing(context.Background(), f.cfg, f.store, f.providers, pool.Name, 0, expected, confirm); err == nil {
				t.Fatal("unverified absence released resource ownership")
			}
			if !reflect.DeepEqual(before, f.store.Snapshot()) || f.host.mutations() != mutations {
				t.Fatal("rejected recovery changed durable state or provider resources")
			}
		})
	}
}

func TestAuditedMissingProbePreservesRemainingWindowWithinHostLimit(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", "protected")}
	f.step(t, 0)
	f.step(t, 15*time.Second)
	key := state.PendingProbeKey(pool.RunnerID, 1, 1)
	deadline := f.store.Snapshot().PendingProbes[key].Deadline
	id := f.store.Snapshot().Pools[pool.Name].Slots[0].ResourceID
	delete(f.host.resources, id)
	f.step(t, 15*time.Second)
	remaining := deadline.Sub(f.now)
	if _, err := ReleaseMissing(context.Background(), f.cfg, f.store, f.providers, pool.Name, 0, id, true); err != nil {
		t.Fatal(err)
	}
	probe := f.store.Snapshot().PendingProbes[key]
	if probe.Exhausted || !probe.Deadline.IsZero() || probe.Remaining != remaining || probe.HostAttempts != 1 || probe.ResourceID != "" {
		t.Fatalf("audited missing probe lost its unspent opportunity: %+v", probe)
	}
	f.restart(t)
	f.step(t, time.Minute)
	f.step(t, 15*time.Second)
	probe = f.store.Snapshot().PendingProbes[key]
	if f.host.creates != 2 || f.host.resumes != 0 || probe.HostAttempts != 2 || probe.Exhausted || !probe.Deadline.Equal(f.now.Add(remaining)) {
		t.Fatalf("audited probe loss did not resume the remaining bounded opportunity: %+v", probe)
	}
}

func TestRetryPendingRequiresConfirmationAndPreservesSpentAttemptUntilReset(t *testing.T) {
	pool := testPool("protected", 1, 1, 0)
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.ProtectedOnly = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", "protected")}
	f.step(t, 0)
	f.step(t, 15*time.Second)
	key := state.PendingProbeKey(pool.RunnerID, 1, 1)
	probe := f.store.Snapshot().PendingProbes[key]
	f.now = probe.Deadline.Add(time.Second)
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	before := f.store.Snapshot()
	if !before.PendingProbes[key].Exhausted {
		t.Fatal("test did not exhaust the initial probe")
	}
	if _, err := RetryPending(f.cfg, f.store, pool.Name, 1, 1, false); err == nil {
		t.Fatal("pending retry did not require explicit confirmation")
	}
	if !reflect.DeepEqual(before, f.store.Snapshot()) {
		t.Fatal("unconfirmed retry reset a spent attempt")
	}
	data, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	mutations := f.host.mutations()
	backup, err := RetryPending(f.cfg, f.store, pool.Name, 1, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(backup)
	if err != nil || string(copied) != string(data) {
		t.Fatal("pending retry failed to back up the exact spent attempt")
	}
	after := f.store.Snapshot()
	if f.host.mutations() != mutations || after.PendingProbes[key].Exhausted || len(after.Audit) != 1 || after.Audit[0].Action != "retry-pending" {
		t.Fatal("confirmed retry did not reset the attempt with an audit record and no provider mutation")
	}
	f.restart(t)
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	if current, exists := f.store.Snapshot().PendingProbes[key]; !exists || current.Exhausted {
		t.Fatal("explicit retry did not permit a new bounded probe after restart")
	}
}

func TestRetryPendingRejectsActiveOrMissingJobsAndUnspentProbes(t *testing.T) {
	for _, reason := range []string{"missing-job", "running-job", "unspent-probe", "wrong-runner"} {
		t.Run(reason, func(t *testing.T) {
			pool := testPool("protected", 1, 1, 0)
			f := setup(t, pool)
			key := state.PendingProbeKey(pool.RunnerID, 1, 1)
			if err := f.store.Update(func(saved *state.State) error {
				saved.Jobs["1/1"] = job(1, "pending", "protected")
				probe := state.PendingProbe{ProjectID: 1, JobID: 1, RunnerID: pool.RunnerID, Pool: pool.Name, Slot: 0, ReservedAt: f.now, Exhausted: true}
				switch reason {
				case "missing-job":
					delete(saved.Jobs, "1/1")
				case "running-job":
					saved.Jobs["1/1"] = job(1, "running", "protected")
				case "unspent-probe":
					probe.Exhausted = false
				case "wrong-runner":
					probe.RunnerID = poolRunnerID("different")
					key = state.PendingProbeKey(probe.RunnerID, 1, 1)
				}
				saved.PendingProbes = map[string]state.PendingProbe{key: probe}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := f.store.Snapshot()
			if _, err := RetryPending(f.cfg, f.store, pool.Name, 1, 1, true); err == nil {
				t.Fatal("retry reset an ineligible pending attempt")
			}
			if !reflect.DeepEqual(before, f.store.Snapshot()) || f.host.mutations() != 0 {
				t.Fatal("rejected retry changed state or provider resources")
			}
		})
	}
}
