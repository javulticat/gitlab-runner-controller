package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/gitlab"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

func TestUntaggedRunnerPolicyBlocksPendingAllocationWithDiagnostic(t *testing.T) {
	pool := testPool("linux", 1, 1, 0)
	f := setup(t, pool)
	policy := f.api.policies[pool.RunnerID]
	policy.RunUntagged = true
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, 0)
	saved := f.store.Snapshot()
	if f.host.mutations() != 0 || f.c.demands(saved)[pool.Name].Pending != 0 || len(saved.PendingProbes) != 0 {
		t.Fatal("runner that accepts untagged work allocated pending capacity")
	}
	if diagnostic := saved.Pools[pool.Name].RunnerError; !strings.Contains(diagnostic, "disable untagged jobs") {
		t.Fatalf("runner policy diagnostic lacks corrective action: %q", diagnostic)
	}
}

func TestAssignedWorkSurvivesRunnerPolicyFailure(t *testing.T) {
	for _, known := range []bool{false, true} {
		name := "unknown-metadata"
		if known {
			name = "untagged-runner"
		}
		t.Run(name, func(t *testing.T) {
			pool := testPool("linux", 1, 1, 0)
			pool.AdoptIDs = []string{"owned"}
			f := setup(t, pool)
			policy := f.api.policies[pool.RunnerID]
			policy.Known, policy.RunUntagged = known, true
			f.api.policies[pool.RunnerID] = policy
			busy := job(1, "running", "linux")
			busy.Tags = []string{"changed-after-assignment"}
			f.api.jobs = []state.Job{busy}
			f.step(t, 0)
			f.step(t, time.Minute)
			f.step(t, time.Minute)
			if f.host.stops != 0 || f.host.destroys != 0 || f.host.creates != 0 || f.host.resumes != 0 {
				t.Fatal("runner policy failure changed capacity with assigned work")
			}
		})
	}
}

func TestRunnerPolicyRequestFailuresRemainVisible(t *testing.T) {
	for _, known := range []bool{false, true} {
		name := "metadata-query"
		if known {
			name = "project-availability"
		}
		t.Run(name, func(t *testing.T) {
			pool := testPool("linux", 1, 1, 0)
			f := setup(t, pool)
			policy := f.api.policies[pool.RunnerID]
			policy.Known = known
			policy.Error = "GitLab runner metadata query failed: GitLab returned HTTP 403"
			if known {
				delete(policy.Projects, 1)
				policy.Error = "GitLab project 1 runner availability check failed: GitLab returned HTTP 403"
			}
			f.api.policies[pool.RunnerID] = policy
			f.api.jobs = []state.Job{job(1, "pending", "linux")}
			f.step(t, 0)
			if diagnostic := f.store.Snapshot().Pools[pool.Name].RunnerError; diagnostic != policy.Error {
				t.Fatalf("safe operation diagnostic was lost: got %q, want %q", diagnostic, policy.Error)
			}
			if f.host.mutations() != 0 {
				t.Fatal("failed policy request allocated pending capacity")
			}
		})
	}
}

func TestUnknownProjectAvailabilityDoesNotBlockKnownProjects(t *testing.T) {
	pool := testPool("linux", 1, 1, 0)
	f := setup(t, pool)
	f.cfg.ProjectIDs = append(f.cfg.ProjectIDs, 2)
	f.construct(t)
	policy := f.api.policies[pool.RunnerID]
	policy.Error = "GitLab project 2 runner availability check failed: GitLab returned HTTP 403"
	f.api.policies[pool.RunnerID] = policy
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, 0)
	if f.host.creates != 1 {
		t.Fatal("unknown project availability blocked an independently verified project")
	}
	if diagnostic := f.store.Snapshot().Pools[pool.Name].RunnerError; diagnostic != policy.Error {
		t.Fatalf("unknown project diagnostic was lost while known project scheduled: %q", diagnostic)
	}
}

func TestRunnerPolicyErrorDoesNotTrustMismatchedRunner(t *testing.T) {
	pool := testPool("linux", 1, 1, 0)
	policy := gitlab.RunnerPolicy{ID: pool.RunnerID + 1, Error: "unrelated runner diagnostic"}
	if diagnostic := policyError(pool, policy); diagnostic != "runner metadata is unknown or inaccessible" {
		t.Fatalf("mismatched runner supplied diagnostic: %q", diagnostic)
	}
}
