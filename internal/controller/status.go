package controller

import (
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type Status struct {
	Ready     bool        `json:"ready"`
	LastStep  time.Time   `json:"last_step,omitempty"`
	LastError string      `json:"last_error,omitempty"`
	State     state.State `json:"state"`
}

func (c *Controller) Status() Status {
	saved := c.store.Snapshot()
	c.statusMu.RLock()
	result := Status{LastStep: c.lastStep, LastError: c.lastError, State: saved}
	c.statusMu.RUnlock()
	result.Ready = !c.stopping.Load() && c.store.Healthy() && c.verified(saved) && result.LastError == "" && !result.LastStep.IsZero() &&
		c.now().Sub(saved.LastSync) <= time.Duration(c.cfg.JobsPollInterval)*2 && c.now().Sub(result.LastStep) < 6*time.Minute
	for _, pool := range saved.Pools {
		if pool.RunnerError != "" {
			result.Ready = false
		}
		for _, slot := range pool.Slots {
			if slot.LastError != "" || bootstrapExpired(slot, c.now()) || slot.Power == provider.Unknown || (slot.Power == provider.Running && !slot.Ready) || (slot.Operation != nil && (slot.Operation.Kind == "unresolved" || slot.Operation.Kind == "create")) {
				result.Ready = false
			}
		}
	}
	for name, until := range saved.ProviderBackoff {
		if c.now().Before(until) || saved.ProviderErrors[name] != "" {
			result.Ready = false
		}
	}
	if len(saved.ProviderErrors) > 0 {
		result.Ready = false
	}
	for _, probe := range saved.PendingProbes {
		if probe.Exhausted && probe.ExhaustionReason == "host_limit" {
			result.Ready = false
		}
	}
	return result
}
