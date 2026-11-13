package state

import (
	"testing"
	"time"
)

func TestValidatePendingProbeHostAccounting(t *testing.T) {
	valid := PendingProbe{ProjectID: 1, JobID: 2, RunnerID: 1, Pool: "linux", ReservedAt: time.Unix(1_800_000_000, 0), Remaining: time.Minute, HostAttempts: 2}
	for _, reason := range []string{"valid", "zero-attempts", "negative-attempts", "unknown-exhaustion", "opportunity_budget", "host_limit", "pool_removed"} {
		t.Run(reason, func(t *testing.T) {
			saved, probe := validSavedState(), valid
			wantValid := true
			switch reason {
			case "zero-attempts":
				probe.HostAttempts = 0
			case "negative-attempts":
				probe.HostAttempts, wantValid = -1, false
			case "unknown-exhaustion":
				probe.Exhausted, probe.ExhaustionReason, wantValid = true, "arbitrary", false
			case "opportunity_budget", "host_limit", "pool_removed":
				probe.Exhausted, probe.ExhaustionReason = true, reason
			}
			saved.PendingProbes = map[string]PendingProbe{PendingProbeKey(probe.RunnerID, probe.ProjectID, probe.JobID): probe}
			if err := saved.Validate(); (err == nil) != wantValid {
				t.Fatalf("probe validation error = %v, want valid %v", err, wantValid)
			}
		})
	}
}
