package controller

import (
	"errors"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func activeProbe(saved state.State, runnerID int64) string {
	for key, probe := range saved.PendingProbes {
		if probe.RunnerID == runnerID && !probe.Exhausted {
			return key
		}
	}
	return ""
}

func (c *Controller) claimProbe(saved *state.State, pool config.Pool, index int) {
	d := c.demands(*saved)[pool.Name]
	if d.ProbeKey == "" {
		return
	}
	if saved.PendingProbes == nil {
		saved.PendingProbes = map[string]state.PendingProbe{}
	}
	probe, exists := saved.PendingProbes[d.ProbeKey]
	if !exists {
		for _, job := range saved.Jobs {
			if state.PendingProbeKey(pool.RunnerID, job.ProjectID, job.ID) == d.ProbeKey {
				probe = state.PendingProbe{ProjectID: job.ProjectID, JobID: job.ID, RunnerID: pool.RunnerID, Pool: pool.Name, Slot: index, ReservedAt: c.now(), Remaining: c.cfg.ProbeTimeout()}
				break
			}
		}
	}
	if probe.ResourceID == "" {
		probe.Pool = pool.Name
		probe.Slot = index
		probe.ResourceID = saved.Pools[pool.Name].Slots[index].ResourceID
	}
	saved.PendingProbes[d.ProbeKey] = probe
}

func readyCapacity(pool state.Pool, configured int) int {
	capacity := 0
	for _, slot := range pool.Slots {
		if slot.Power == provider.Running && slot.Ready && slot.LastError == "" && slot.Operation == nil {
			jobs := slot.ConfiguredJobs
			if jobs == 0 {
				jobs = min(1, configured)
			}
			capacity += jobs
		}
	}
	return capacity
}

func (c *Controller) updateProbes(inventory map[string]map[string]provider.Resource) error {
	return c.store.Update(func(saved *state.State) error {
		if !c.verified(*saved) {
			return state.ErrStale
		}
		demands := c.demands(*saved)
		for _, pool := range c.cfg.Pools {
			if _, available := inventory[pool.Provider]; !available || !c.providerVerified(*saved, pool.Provider) {
				continue
			}
			d := demands[pool.Name]
			if d.ProbeKey != "" {
				if _, exists := saved.PendingProbes[d.ProbeKey]; !exists && readyCapacity(saved.Pools[pool.Name], pool.JobsPerInstance) > d.Busy+d.UnknownBusy {
					for index, slot := range saved.Pools[pool.Name].Slots {
						if slot.ResourceID != "" && slot.Power == provider.Running && slot.Operation == nil && slot.Ready {
							c.claimProbe(saved, pool, index)
							break
						}
					}
				}
			}
		}
		for key, probe := range saved.PendingProbes {
			if probe.Exhausted {
				continue
			}
			pool := c.poolByName(probe.Pool)
			if pool.Name == "" || pool.RunnerID != probe.RunnerID {
				probe.Exhausted = true
				probe.ExhaustionReason = "pool_removed"
				saved.PendingProbes[key] = probe
				continue
			}
			_, available := inventory[pool.Provider]
			d := demands[pool.Name]
			policy := c.runnerPolicies[pool.RunnerID]
			projectAvailable, projectKnown := policy.Projects[probe.ProjectID]
			opportunity := available && c.providerVerified(*saved, pool.Provider) && policyError(pool, policy) == "" && projectKnown && projectAvailable && pool.ProtectedPendingPolicy != "strict" && readyCapacity(saved.Pools[pool.Name], pool.JobsPerInstance) > d.Busy+d.UnknownBusy
			if available && c.providerVerified(*saved, pool.Provider) && probe.ResourceID == "" && !opportunity && exhaustHostLimit(saved, pool, key) {
				continue
			}
			if !opportunity {
				if !probe.Deadline.IsZero() {
					probe.Remaining = max(0, probe.Deadline.Sub(c.now()))
					probe.Deadline = time.Time{}
				}
			} else {
				if probe.Deadline.IsZero() {
					probe.Deadline = c.now().Add(probe.Remaining)
				}
				if !c.now().Before(probe.Deadline) && !saved.LastSync.Before(probe.Deadline) {
					probe.Exhausted = true
					probe.ExhaustionReason = "opportunity_budget"
					probe.Remaining = 0
				}
			}
			saved.PendingProbes[key] = probe
		}
		return nil
	})
}

func releaseResourceProbes(saved *state.State, runnerID int64, resourceID string, now time.Time, keepOpportunity bool) {
	for key, probe := range saved.PendingProbes {
		if probe.RunnerID == runnerID && probe.ResourceID == resourceID && !probe.Exhausted {
			if !keepOpportunity {
				if !probe.Deadline.IsZero() {
					probe.Remaining = min(probe.Remaining, max(0, probe.Deadline.Sub(now)))
				}
				probe.Deadline = time.Time{}
			}
			probe.ResourceID = ""
			saved.PendingProbes[key] = probe
		}
	}
}

func associateProbeHost(saved *state.State, pool state.Pool, poolName string, index int, id, key string) {
	if key == "" {
		return
	}
	probe, found := saved.PendingProbes[key]
	if !found || probe.Exhausted || probe.RunnerID != pool.RunnerID || probe.Pool != poolName || probe.Slot != index {
		return
	}
	probe.ResourceID = id
	probe.HostAttempts++
	saved.PendingProbes[key] = probe
}

func (c *Controller) otherProbeOpportunity(saved state.State, runnerID int64, excludedID string) bool {
	for _, pool := range c.cfg.Pools {
		if pool.RunnerID == runnerID {
			capacity := 0
			for _, slot := range saved.Pools[pool.Name].Slots {
				if slot.ResourceID != excludedID && slot.Power == provider.Running && slot.Ready && slot.LastError == "" && slot.Operation == nil {
					jobs := slot.ConfiguredJobs
					if jobs == 0 {
						jobs = 1
					}
					capacity += jobs
				}
			}
			d := c.demands(saved)[pool.Name]
			return capacity > d.Busy+d.UnknownBusy
		}
	}
	return false
}

func (c *Controller) updateIdle(demands map[string]demand) error {
	return c.store.Update(func(saved *state.State) error {
		if !c.verified(*saved) {
			return state.ErrStale
		}
		for _, pool := range c.cfg.Pools {
			p := saved.Pools[pool.Name]
			if !demands[pool.Name].idle() {
				p.IdleSince = time.Time{}
			} else if p.IdleSince.IsZero() {
				p.IdleSince = c.now()
			}
			if demands[pool.Name].idle() {
				for index, slot := range p.Slots {
					if slot.ResourceID == "" && slot.Operation == nil {
						p.Slots[index] = state.Slot{Power: provider.Stopped}
					} else if slot.Power == provider.Stopped && slot.Operation == nil && (!slot.BootstrapStartedAt.IsZero() || !slot.BootstrapDeadline.IsZero()) {
						slot.BootstrapStartedAt, slot.BootstrapDeadline = time.Time{}, time.Time{}
						slot.LastError = ""
						slot.RetryAt = time.Time{}
						p.Slots[index] = slot
					}
				}
			}
			saved.Pools[pool.Name] = p
		}
		return nil
	})
}

func (c *Controller) ensureBootstrap(pool string, index int) error {
	return c.slotUpdate(pool, index, func(slot *state.Slot) {
		if !slot.Ready && slot.BootstrapDeadline.IsZero() {
			slot.BootstrapStartedAt = c.now()
			slot.BootstrapDeadline = c.now().Add(time.Duration(c.cfg.ReadinessTimeout))
		}
	})
}

var errRetryConfirmation = errors.New("retry-pending requires --confirm-retry after auditing eligibility and spending")
