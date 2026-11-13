package controller

import (
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
)

func exhaustHostLimit(saved *state.State, pool config.Pool, key string) bool {
	probe, found := saved.PendingProbes[key]
	if !found || probe.HostAttempts < pool.ProbeMaxHosts() {
		return false
	}
	if probe.Exhausted {
		return true
	}
	probe.Exhausted = true
	probe.ExhaustionReason = "host_limit"
	probe.Deadline = time.Time{}
	saved.PendingProbes[key] = probe
	return true
}

func (c *Controller) limitProbeHosts(pool config.Pool, key string) (bool, error) {
	blocked := false
	err := c.store.Update(func(saved *state.State) error { blocked = exhaustHostLimit(saved, pool, key); return nil })
	return blocked, err
}
