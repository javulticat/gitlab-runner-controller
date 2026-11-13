package state

import (
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func validSavedState() State {
	saved := New("controller", "gitlab-projects")
	saved.ProviderBindings["host"] = provider.Description{Identity: "account", Scope: "region", Lifecycle: provider.Retained, Revision: "1"}
	saved.Pools["linux"] = Pool{Provider: "host", OptionsHash: "options", RunnerID: 1,
		Slots: []Slot{{ResourceID: "owned", Power: provider.Running}}}
	return saved
}

func TestValidateRejectsInvalidOwnershipAndOperationState(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name   string
		change func(*State)
	}{
		{"unsupported-version", func(saved *State) { saved.Version++ }},
		{"missing-controller", func(saved *State) { saved.ControllerID = "" }},
		{"missing-gitlab-binding", func(saved *State) { saved.Binding = "" }},
		{"missing-pools", func(saved *State) { saved.Pools = nil }},
		{"missing-jobs", func(saved *State) { saved.Jobs = nil }},
		{"missing-hints", func(saved *State) { saved.Hints = nil }},
		{"missing-provider-backoff", func(saved *State) { saved.ProviderBackoff = nil }},
		{"missing-provider-bindings", func(saved *State) { saved.ProviderBindings = nil }},
		{"unverified-future-hints", func(saved *State) { saved.VerifiedRevision = saved.HintRevision + 1 }},
		{"unbound-resource", func(saved *State) { delete(saved.ProviderBindings, "host") }},
		{"invalid-binding", func(saved *State) {
			binding := saved.ProviderBindings["host"]
			binding.Scope = ""
			saved.ProviderBindings["host"] = binding
		}},
		{"duplicate-account-alias", func(saved *State) { saved.ProviderBindings["alias"] = saved.ProviderBindings["host"] }},
		{"missing-pool-name", func(saved *State) { saved.Pools[""] = saved.Pools["linux"] }},
		{"missing-pool-provider", func(saved *State) { changeSavedPool(saved, func(pool *Pool) { pool.Provider = "" }) }},
		{"missing-options-binding", func(saved *State) { changeSavedPool(saved, func(pool *Pool) { pool.OptionsHash = "" }) }},
		{"missing-runner-binding", func(saved *State) { changeSavedPool(saved, func(pool *Pool) { pool.RunnerID = 0 }) }},
		{"negative-runner-binding", func(saved *State) { changeSavedPool(saved, func(pool *Pool) { pool.RunnerID = -1 }) }},
		{"missing-slots", func(saved *State) { changeSavedPool(saved, func(pool *Pool) { pool.Slots = nil }) }},
		{"excess-slots", func(saved *State) { changeSavedPool(saved, func(pool *Pool) { pool.Slots = make([]Slot, 1001) }) }},
		{"duplicate-resource", func(saved *State) {
			changeSavedPool(saved, func(pool *Pool) { pool.Slots = append(pool.Slots, pool.Slots[0]) })
		}},
		{"invalid-power", func(saved *State) { changeSavedSlot(saved, func(slot *Slot) { slot.Power = "starting" }) }},
		{"running-without-resource", func(saved *State) { changeSavedSlot(saved, func(slot *Slot) { slot.ResourceID = "" }) }},
		{"ready-without-resource", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.ResourceID, slot.Power, slot.Ready = "", provider.Stopped, true })
		}},
		{"ready-stopped-resource", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.Power, slot.Ready = provider.Stopped, true })
		}},
		{"ready-during-release", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) {
				slot.Ready = true
				slot.Operation = &Operation{Kind: "stop", ID: "op", StartedAt: started}
			})
		}},
		{"negative-configured-concurrency", func(saved *State) { changeSavedSlot(saved, func(slot *Slot) { slot.ConfiguredJobs = -1 }) }},
		{"excess-configured-concurrency", func(saved *State) { changeSavedSlot(saved, func(slot *Slot) { slot.ConfiguredJobs = 65 }) }},
		{"bootstrap-deadline-without-start", func(saved *State) { changeSavedSlot(saved, func(slot *Slot) { slot.BootstrapDeadline = started }) }},
		{"bootstrap-deadline-before-start", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.BootstrapStartedAt, slot.BootstrapDeadline = started, started.Add(-time.Second) })
		}},
		{"unknown-operation", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "reboot", ID: "op", StartedAt: started} })
		}},
		{"missing-operation-id", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "stop", StartedAt: started} })
		}},
		{"missing-operation-time", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "stop", ID: "op"} })
		}},
		{"unknown-operation-phase", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) {
				slot.Operation = &Operation{Kind: "stop", ID: "op", StartedAt: started, Phase: "complete"}
			})
		}},
		{"drained-resume", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) {
				slot.Operation = &Operation{Kind: "resume", ID: "op", StartedAt: started, Phase: "drained"}
			})
		}},
		{"create-already-has-resource", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "create", ID: "op", StartedAt: started} })
		}},
		{"unresolved-already-has-resource", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "unresolved", ID: "op", StartedAt: started} })
		}},
		{"unresolved-release-phase", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) {
				slot.ResourceID = ""
				slot.Power = provider.Stopped
				slot.Operation = &Operation{Kind: "unresolved", ID: "op", StartedAt: started, Phase: "requested"}
			})
		}},
		{"release-without-resource", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) {
				slot.ResourceID = ""
				slot.Power = provider.Stopped
				slot.Operation = &Operation{Kind: "stop", ID: "op", StartedAt: started}
			})
		}},
		{"retained-destroy", func(saved *State) {
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "destroy", ID: "op", StartedAt: started} })
		}},
		{"disposable-stop", func(saved *State) {
			setSavedLifecycle(saved, provider.Disposable)
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "stop", ID: "op", StartedAt: started} })
		}},
		{"disposable-resume", func(saved *State) {
			setSavedLifecycle(saved, provider.Disposable)
			changeSavedSlot(saved, func(slot *Slot) { slot.Operation = &Operation{Kind: "resume", ID: "op", StartedAt: started} })
		}},
		{"job-key-mismatch", func(saved *State) { saved.Jobs["1/2"] = Job{ProjectID: 1, ID: 1} }},
		{"invalid-job-project", func(saved *State) { saved.Jobs["0/1"] = Job{ID: 1} }},
		{"invalid-job-id", func(saved *State) { saved.Jobs["1/0"] = Job{ProjectID: 1} }},
		{"hint-key-mismatch", func(saved *State) { saved.Hints["1/2"] = Hint{ProjectID: 1, ID: 1} }},
		{"invalid-hint-project", func(saved *State) { saved.Hints["0/1"] = Hint{ID: 1} }},
		{"invalid-hint-id", func(saved *State) { saved.Hints["1/0"] = Hint{ProjectID: 1} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saved := validSavedState()
			if err := saved.Validate(); err != nil {
				t.Fatal("invalid test fixture:", err)
			}
			tc.change(&saved)
			if err := saved.Validate(); err == nil {
				t.Fatal("invalid durable state was accepted")
			}
		})
	}
}

func TestValidateAcceptsDurableLifecycleTransitions(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name      string
		lifecycle provider.Lifecycle
		kind      string
		phase     string
	}{
		{"create", provider.Retained, "create", ""},
		{"unresolved-create", provider.Retained, "unresolved", ""},
		{"await-running", provider.Disposable, "await-running", "requested"},
		{"resume", provider.Retained, "resume", ""},
		{"drain-before-stop", provider.Retained, "stop", ""},
		{"drained-before-stop", provider.Retained, "stop", "drained"},
		{"stop-awaits-observation", provider.Retained, "stop", "requested"},
		{"drain-before-destroy", provider.Disposable, "destroy", ""},
		{"drained-before-destroy", provider.Disposable, "destroy", "drained"},
		{"destroy-awaits-absence", provider.Disposable, "destroy", "requested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saved := validSavedState()
			setSavedLifecycle(&saved, tc.lifecycle)
			changeSavedSlot(&saved, func(slot *Slot) {
				slot.Operation = &Operation{Kind: tc.kind, ID: "operation", StartedAt: started, Phase: tc.phase}
				if tc.kind == "create" || tc.kind == "unresolved" {
					slot.ResourceID, slot.Power = "", provider.Stopped
				}
			})
			if err := saved.Validate(); err != nil {
				t.Fatal("valid lifecycle transition was rejected:", err)
			}
		})
	}
}

func TestResourceIdentityIsScopedToProviderAccount(t *testing.T) {
	saved := validSavedState()
	saved.ProviderBindings["other"] = provider.Description{Identity: "other-account", Scope: "region", Lifecycle: provider.Retained, Revision: "1"}
	other := saved.Pools["linux"]
	other.Provider, other.RunnerID = "other", 2
	other.Slots = []Slot{{ResourceID: "owned", Power: provider.Running}}
	saved.Pools["other"] = other
	if err := saved.Validate(); err != nil {
		t.Fatal("same resource ID in independent accounts was rejected:", err)
	}
	other.Provider = "host"
	saved.Pools["other"] = other
	if err := saved.Validate(); err == nil {
		t.Fatal("two pools claimed the same resource in one account")
	}
}

func TestValidateRecoveryHistory(t *testing.T) {
	valid := AuditRecord{ID: "audit", At: time.Unix(1_800_000_000, 0), Action: "release-missing", Pool: "removed-pool", ResourceID: "removed-host", Binding: validSavedState().ProviderBindings["host"]}
	for _, action := range []string{"release-missing", "retry-pending"} {
		saved := validSavedState()
		record := valid
		record.Action = action
		saved.Audit = []AuditRecord{record}
		if err := saved.Validate(); err != nil {
			t.Fatal("valid historical recovery record was rejected:", err)
		}
	}
	for _, reason := range []string{"missing-id", "missing-time", "unknown-action", "missing-pool", "negative-slot", "invalid-binding", "excess-history"} {
		t.Run(reason, func(t *testing.T) {
			saved, record := validSavedState(), valid
			switch reason {
			case "missing-id":
				record.ID = ""
			case "missing-time":
				record.At = time.Time{}
			case "unknown-action":
				record.Action = "replace"
			case "missing-pool":
				record.Pool = ""
			case "negative-slot":
				record.Slot = -1
			case "invalid-binding":
				record.Binding.Identity = ""
			}
			saved.Audit = []AuditRecord{record}
			if reason == "excess-history" {
				for range 1000 {
					saved.Audit = append(saved.Audit, record)
				}
			}
			if err := saved.Validate(); err == nil {
				t.Fatal("invalid recovery history was accepted")
			}
		})
	}
}

func TestValidatePendingProbeIdentityAndSingleActiveAttempt(t *testing.T) {
	valid := PendingProbe{ProjectID: 1, JobID: 2, RunnerID: 3, Pool: "prior-pool-name", Slot: 0, ReservedAt: time.Unix(1_800_000_000, 0), Remaining: time.Minute}
	for _, reason := range []string{"valid", "wrong-key", "missing-runner", "missing-project", "missing-job", "missing-pool", "negative-slot", "missing-reservation", "negative-remaining", "duplicate-active-runner", "independent-runner", "spent-attempt"} {
		t.Run(reason, func(t *testing.T) {
			saved, probe := validSavedState(), valid
			switch reason {
			case "missing-runner":
				probe.RunnerID = 0
			case "missing-project":
				probe.ProjectID = 0
			case "missing-job":
				probe.JobID = 0
			case "missing-pool":
				probe.Pool = ""
			case "negative-slot":
				probe.Slot = -1
			case "missing-reservation":
				probe.ReservedAt = time.Time{}
			case "negative-remaining":
				probe.Remaining = -time.Second
			}
			key := PendingProbeKey(probe.RunnerID, probe.ProjectID, probe.JobID)
			if reason == "wrong-key" {
				key = "1/2/prior-pool-name"
			}
			saved.PendingProbes = map[string]PendingProbe{key: probe}
			if reason == "duplicate-active-runner" || reason == "independent-runner" || reason == "spent-attempt" {
				other := valid
				other.JobID++
				if reason == "independent-runner" {
					other.RunnerID++
				}
				other.Exhausted = reason == "spent-attempt"
				saved.PendingProbes[PendingProbeKey(other.RunnerID, other.ProjectID, other.JobID)] = other
			}
			wantValid := reason == "valid" || reason == "independent-runner" || reason == "spent-attempt"
			if err := saved.Validate(); (err == nil) != wantValid {
				t.Fatalf("probe validation error = %v, want valid %v", err, wantValid)
			}
		})
	}
}

func changeSavedPool(saved *State, change func(*Pool)) {
	pool := saved.Pools["linux"]
	change(&pool)
	saved.Pools["linux"] = pool
}

func changeSavedSlot(saved *State, change func(*Slot)) {
	changeSavedPool(saved, func(pool *Pool) { change(&pool.Slots[0]) })
}

func setSavedLifecycle(saved *State, lifecycle provider.Lifecycle) {
	binding := saved.ProviderBindings["host"]
	binding.Lifecycle = lifecycle
	binding.AbsenceConfirmsDestroy = lifecycle == provider.Disposable
	saved.ProviderBindings["host"] = binding
}
