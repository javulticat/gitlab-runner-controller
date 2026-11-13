package controller

import (
	"context"
	"testing"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func TestUnverifiedAdoptionDoesNotBlockIndependentAccount(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "missing"
		if unknown {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) {
			blocked := testPool("blocked", 1, 1, 0)
			blocked.AdoptIDs = []string{"unverified"}
			f := setup(t, blocked)
			delete(f.host.resources, "unverified")
			if unknown {
				f.host.resources["unverified"] = provider.Resource{ID: "unverified", State: provider.Unknown}
			}
			available := testPool("available", 1, 1, 0)
			available.Provider = "other"
			other := newFakeProvider(provider.Retained)
			other.description.Identity = "account-other"
			spec := testProvider(provider.Retained, 1)
			spec.Identity = other.description.Identity
			f.cfg.Providers["other"], f.providers["other"] = spec, other
			f.cfg.Pools = append(f.cfg.Pools, available)
			f.construct(t)
			f.api.jobs = []state.Job{job(1, "pending", "available")}
			if err := f.c.Step(context.Background()); err == nil {
				t.Fatal("unverified adoption was not reported")
			}
			if other.creates != 1 || f.host.mutations() != 0 {
				t.Fatal("unverified adoption stalled or mutated an independent account")
			}
			if f.store.Snapshot().Pools[blocked.Name].Slots[0].ResourceID != "" {
				t.Fatal("unverified adoption changed resource ownership")
			}
		})
	}
}
