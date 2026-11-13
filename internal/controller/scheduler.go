package controller

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/gitlab"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type demand struct {
	Pending, Busy int
	UnknownBusy   int
	Protected     bool
	ProbeKey      string
}

func (d demand) idle() bool { return d.Pending+d.Busy == 0 && !d.Protected }

func compatible(tags, available []string) bool {
	if len(tags) == 0 {
		return false
	}
	for _, tag := range tags {
		if !slices.Contains(available, tag) {
			return false
		}
	}
	return true
}

func (c *Controller) demands(saved state.State) map[string]demand {
	result := map[string]demand{}
	jobs := make([]state.Job, 0, len(saved.Jobs))
	for _, job := range saved.Jobs {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].ProjectID != jobs[j].ProjectID {
			return jobs[i].ProjectID < jobs[j].ProjectID
		}
		return jobs[i].ID < jobs[j].ID
	})
	for _, job := range jobs {
		if !state.Active(job.Status) {
			continue
		}
		if job.Status != "pending" {
			for _, pool := range c.cfg.Pools {
				d := result[pool.Name]
				if job.RunnerID == pool.RunnerID && job.RunnerID > 0 {
					d.Busy++
				} else if job.RunnerID == 0 && compatible(job.Tags, pool.Tags) {
					d.Protected = true
					d.UnknownBusy++
				}
				result[pool.Name] = d
			}
			continue
		}
		matches := []string{}
		for _, pool := range c.cfg.Pools {
			policy := c.runnerPolicies[pool.RunnerID]
			available, known := policy.Projects[job.ProjectID]
			if policyError(pool, policy) == "" && available && known && compatible(job.Tags, pool.Tags) && (!policy.ProtectedOnly || pool.ProtectedPendingPolicy != "strict") {
				matches = append(matches, pool.Name)
			}
		}
		for _, name := range matches {
			d := result[name]
			if len(matches) != 1 {
				d.Protected = true
			} else {
				pool := c.poolByName(name)
				if !c.runnerPolicies[pool.RunnerID].ProtectedOnly {
					d.Pending++
				} else {
					key := state.PendingProbeKey(pool.RunnerID, job.ProjectID, job.ID)
					probe, exists := saved.PendingProbes[key]
					active := activeProbe(saved, pool.RunnerID)
					if (!exists || !probe.Exhausted) && (active == "" || active == key) && (d.ProbeKey == "" || d.ProbeKey == key || active == key) {
						d.ProbeKey = key
					}
				}
			}
			result[name] = d
		}
	}
	for name, d := range result {
		if d.ProbeKey != "" {
			d.Pending++
		}
		result[name] = d
	}
	return result
}

func policyError(pool config.Pool, policy gitlab.RunnerPolicy) string {
	if !policy.Known || policy.ID != pool.RunnerID {
		if policy.ID == pool.RunnerID && policy.Error != "" {
			return policy.Error
		}
		return "runner metadata is unknown or inaccessible"
	}
	if policy.Paused {
		return "runner is paused"
	}
	if policy.RunUntagged {
		return "runner accepts untagged jobs; disable untagged jobs in the GitLab runner settings"
	}
	actual, expected := slices.Clone(policy.Tags), slices.Clone(pool.Tags)
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		return "configured tags do not match the registered runner"
	}
	return ""
}

func (c *Controller) poolByName(name string) config.Pool {
	for _, pool := range c.cfg.Pools {
		if pool.Name == name {
			return pool
		}
	}
	return config.Pool{}
}

func capacity(saved state.State, inventory map[string]map[string]provider.Resource, providerName string) int {
	used := 0
	resources := inventory[providerName]
	for _, resource := range resources {
		if resource.State != provider.Stopped {
			used++
		}
	}
	for _, pool := range saved.Pools {
		if pool.Provider != providerName {
			continue
		}
		for _, slot := range pool.Slots {
			if slot.ResourceID == "" {
				if slot.Operation != nil {
					used++
				}
				continue
			}
			resource, found := resources[slot.ResourceID]
			if !found || (resource.State == provider.Stopped && slot.Operation != nil && slot.Operation.Kind != "stop") {
				used++
			}
		}
	}
	return used
}

func (c *Controller) slotUpdate(poolName string, index int, change func(*state.Slot)) error {
	return c.store.Update(func(saved *state.State) error {
		p := saved.Pools[poolName]
		change(&p.Slots[index])
		saved.Pools[poolName] = p
		return nil
	})
}

func (c *Controller) resumeRetained(ctx context.Context, pool config.Pool, index int, id string) error {
	return c.mutate(ctx, pool.Provider, func(ctx context.Context) error {
		stamp := func(slot *state.Slot) { slot.ChangedAt = c.now() }
		// Preserve the reservation across crashes and uncertain resume replies.
		if err := c.slotUpdate(pool.Name, index, stamp); err != nil {
			return err
		}
		callErr := c.providers[pool.Provider].Resume(ctx, id)
		return errors.Join(callErr, c.slotUpdate(pool.Name, index, stamp))
	})
}

func bootstrapExpired(slot state.Slot, now time.Time) bool {
	return !slot.Ready && !slot.BootstrapDeadline.IsZero() && !now.Before(slot.BootstrapDeadline)
}

func (c *Controller) bootstrapBackoff() time.Duration {
	return max(time.Duration(c.cfg.ObservationGrace), time.Duration(c.cfg.ReadinessTimeout))
}

func (c *Controller) deferBootstrap(slot *state.Slot, now time.Time) {
	if slot.LastError == "" {
		slot.LastError = "runner readiness deadline exceeded"
	}
	retryAt := now.Add(c.bootstrapBackoff())
	if slot.RetryAt.Before(retryAt) {
		slot.RetryAt = retryAt
	}
}

func (c *Controller) failure(pool string, index int, err error, uncertain bool) error {
	if errors.Is(err, ErrStopping) {
		return err
	}
	delay := 30 * time.Second
	var remote *provider.Error
	if errors.As(err, &remote) && remote.RetryAfter > 0 {
		delay = remote.RetryAfter
	}
	c.log.Warn("resource operation deferred", "pool", pool, "slot", index, "error", err)
	return c.store.Update(func(saved *state.State) error {
		p := saved.Pools[pool]
		slot := &p.Slots[index]
		slot.LastError = err.Error()
		slot.RetryAt = c.now().Add(delay)
		if bootstrapExpired(*slot, c.now()) {
			c.deferBootstrap(slot, c.now())
		}
		if uncertain && slot.Operation != nil {
			slot.Operation.Kind = "unresolved"
		}
		if errors.As(err, &remote) && remote.RetryAfter > 0 {
			saved.ProviderBackoff[p.Provider] = c.now().Add(remote.RetryAfter)
		}
		saved.Pools[pool] = p
		return nil
	})
}

func (c *Controller) begin(pool string, index int, kind string, probeKeys ...string) error {
	return c.store.Update(func(saved *state.State) error {
		if c.stopping.Load() {
			return ErrStopping
		}
		if !c.providerVerified(*saved, saved.Pools[pool].Provider) {
			return state.ErrStale
		}
		p := saved.Pools[pool]
		slot := &p.Slots[index]
		slot.Operation = &state.Operation{Kind: kind, ID: operationID(), StartedAt: c.now()}
		slot.Ready = false
		slot.ChangedAt = c.now()
		if kind == "stop" || kind == "destroy" {
			slot.BootstrapStartedAt, slot.BootstrapDeadline = time.Time{}, time.Time{}
		}
		if kind == "create" || kind == "resume" {
			if kind == "create" || slot.BootstrapDeadline.IsZero() {
				slot.BootstrapStartedAt = c.now()
				slot.BootstrapDeadline = c.now().Add(time.Duration(c.cfg.ReadinessTimeout))
			}
			c.claimProbe(saved, c.poolByName(pool), index)
			if kind == "create" && len(probeKeys) > 0 && probeKeys[0] != "" {
				key := probeKeys[0]
				probe := saved.PendingProbes[key]
				probe.Pool = pool
				probe.Slot = index
				probe.ResourceID = ""
				saved.PendingProbes[key] = probe
				slot.Operation.ProbeKey = key
			}
		}
		saved.Pools[pool] = p
		return nil
	})
}

func (c *Controller) schedule(ctx context.Context, inventory map[string]map[string]provider.Resource) error {
	demands := c.demands(c.store.Snapshot())
	if err := c.updateIdle(demands); err != nil {
		return err
	}
	for _, pool := range orderedPools(c.cfg.Pools) {
		if _, available := inventory[pool.Provider]; !available {
			continue
		}
		for index := range c.store.Snapshot().Pools[pool.Name].Slots {
			if c.admissionStopped(ctx) {
				return ErrStopping
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !c.verified(c.store.Snapshot()) {
				return state.ErrStale
			}
			if !c.providerVerified(c.store.Snapshot(), pool.Provider) {
				continue
			}
			slot := c.store.Snapshot().Pools[pool.Name].Slots[index]
			if c.now().Before(slot.RetryAt) {
				continue
			}
			if slot.Operation != nil {
				if slot.Operation.Kind == "stop" || slot.Operation.Kind == "destroy" {
					if err := c.finishRelease(ctx, pool, index); err != nil {
						return err
					}
				} else if slot.Operation.Kind == "resume" && slot.ResourceID != "" && c.now().Sub(slot.ChangedAt) >= time.Duration(c.cfg.ObservationGrace) {
					if err := c.resumeRetained(ctx, pool, index, slot.ResourceID); err != nil {
						if err := c.failure(pool.Name, index, err, false); err != nil {
							return err
						}
					} else if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) {
						if bootstrapExpired(*slot, c.now()) {
							c.deferBootstrap(slot, c.now())
						}
					}); err != nil {
						return err
					}
				}
				continue
			}
			if c.cfg.Providers[pool.Provider].Lifecycle == provider.Disposable && slot.ResourceID != "" && slot.Power == provider.Stopped {
				if err := c.begin(pool.Name, index, "destroy"); err != nil {
					return err
				}
				if err := c.finishRelease(ctx, pool, index); err != nil {
					return err
				}
				continue
			}
			if slot.Power != provider.Running {
				continue
			}
			if err := c.ensureBootstrap(pool.Name, index); err != nil {
				return err
			}
			if !slot.Persisted && c.cfg.Providers[pool.Provider].Lifecycle == provider.Retained {
				if err := c.mutate(ctx, pool.Provider, func(ctx context.Context) error { return c.providers[pool.Provider].Persist(ctx, slot.ResourceID) }); err != nil {
					if err := c.failure(pool.Name, index, err, false); err != nil {
						return err
					}
					continue
				}
				if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) { slot.Persisted = true }); err != nil {
					return err
				}
			}
			spec := c.spec(pool, index)
			if slot.Ready {
				healthy, err := c.providers[pool.Provider].Healthy(ctx, slot.ResourceID, pool.Options, spec.Name)
				if err != nil {
					if err := c.failure(pool.Name, index, err, false); err != nil {
						return err
					}
					continue
				}
				// A healthy manager keeps its configuration until an idle stop and resume.
				if healthy {
					if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) {
						slot.LastError = ""
						slot.RetryAt = time.Time{}
						slot.BootstrapDeadline = time.Time{}
					}); err != nil {
						return err
					}
					continue
				}
				if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) {
					slot.Ready = false
					slot.BootstrapStartedAt = c.now()
					slot.BootstrapDeadline = c.now().Add(time.Duration(c.cfg.ReadinessTimeout))
				}); err != nil {
					return err
				}
			}
			var ready bool
			err := c.mutate(ctx, pool.Provider, func(ctx context.Context) error {
				var callErr error
				ready, callErr = c.providers[pool.Provider].Configure(ctx, slot.ResourceID, pool.Options, spec)
				return callErr
			})
			if err != nil {
				if err := c.failure(pool.Name, index, err, false); err != nil {
					return err
				}
				continue
			}
			if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) {
				slot.Ready = ready
				if ready {
					slot.RuntimeRevision = spec.Revision
					slot.BootstrapDeadline = time.Time{}
					slot.ConfiguredJobs = pool.JobsPerInstance
					slot.LastError = ""
					slot.RetryAt = time.Time{}
				} else if bootstrapExpired(*slot, c.now()) {
					c.deferBootstrap(slot, c.now())
				}
			}); err != nil {
				return err
			}
		}
	}
	if c.admissionStopped(ctx) {
		return ErrStopping
	}
	if err := c.updateProbes(inventory); err != nil {
		return err
	}
	demands = c.demands(c.store.Snapshot())
	if err := c.updateIdle(demands); err != nil {
		return err
	}
	for _, pool := range c.cfg.Pools {
		if _, available := inventory[pool.Provider]; !available || !c.providerVerified(c.store.Snapshot(), pool.Provider) {
			continue
		}
		p := c.store.Snapshot().Pools[pool.Name]
		if !p.IdleSince.IsZero() && c.now().Sub(p.IdleSince) >= time.Duration(c.cfg.IdleGrace) {
			for index, slot := range p.Slots {
				if slot.Power == provider.Running && slot.Operation == nil && !c.now().Before(slot.RetryAt) {
					if err := c.begin(pool.Name, index, c.releaseKind(pool)); err != nil {
						return err
					}
					if err := c.finishRelease(ctx, pool, index); err != nil {
						return err
					}
				}
			}
		}
	}
	for _, pool := range orderedPools(c.cfg.Pools) {
		if c.admissionStopped(ctx) {
			return ErrStopping
		}
		if _, available := inventory[pool.Provider]; !available || !c.providerVerified(c.store.Snapshot(), pool.Provider) {
			continue
		}
		d := demands[pool.Name]
		if d.Pending == 0 {
			continue
		}
		desired := min(pool.MaxInstances, (d.Pending+d.Busy+pool.JobsPerInstance-1)/pool.JobsPerInstance)
		p := c.store.Snapshot().Pools[pool.Name]
		active := 0
		for _, slot := range p.Slots {
			if (slot.Power != provider.Stopped || slot.Operation != nil) && (slot.Operation == nil || (slot.Operation.Kind != "stop" && slot.Operation.Kind != "destroy")) {
				active++
			}
		}
		for index := range pool.MaxInstances {
			if c.admissionStopped(ctx) {
				return ErrStopping
			}
			if active >= desired {
				break
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			saved := c.store.Snapshot()
			if !c.providerVerified(saved, pool.Provider) {
				break
			}
			if capacity(saved, inventory, pool.Provider) >= c.cfg.Providers[pool.Provider].MaxRunning {
				if err := c.reclaim(ctx, pool.Provider, demands); err != nil {
					return err
				}
				break
			}
			slot := saved.Pools[pool.Name].Slots[index]
			if slot.Power != provider.Stopped || slot.Operation != nil || c.now().Before(slot.RetryAt) {
				continue
			}
			if slot.ResourceID != "" && c.cfg.Providers[pool.Provider].Lifecycle == provider.Disposable {
				continue
			}
			kind := "resume"
			if slot.ResourceID == "" {
				kind = "create"
			}
			probeKey := ""
			if kind == "create" && d.ProbeKey != "" {
				knownDesired := min(pool.MaxInstances, (d.Pending-1+d.Busy+pool.JobsPerInstance-1)/pool.JobsPerInstance)
				if active >= knownDesired {
					probeKey = d.ProbeKey
					blocked, err := c.limitProbeHosts(pool, probeKey)
					if err != nil {
						return err
					}
					if blocked {
						break
					}
				}
			}
			if err := c.begin(pool.Name, index, kind, probeKey); err != nil {
				return err
			}
			if err := c.activate(ctx, pool, index, slot.ResourceID); err != nil {
				return err
			}
			active++
		}
	}
	return nil
}

func (c *Controller) activate(ctx context.Context, pool config.Pool, index int, id string) error {
	if id != "" {
		if err := c.resumeRetained(ctx, pool, index, id); err != nil {
			var remote *provider.Error
			if errors.As(err, &remote) && remote.NoSideEffect {
				if saveErr := c.slotUpdate(pool.Name, index, func(slot *state.Slot) { slot.Operation = nil }); saveErr != nil {
					return saveErr
				}
			}
			return c.failure(pool.Name, index, err, false)
		}
		return c.slotUpdate(pool.Name, index, func(slot *state.Slot) {
			if bootstrapExpired(*slot, c.now()) {
				c.deferBootstrap(slot, c.now())
			}
		})
	}
	slot := c.store.Snapshot().Pools[pool.Name].Slots[index]
	var resource provider.Resource
	err := c.mutate(ctx, pool.Provider, func(ctx context.Context) error {
		var callErr error
		resource, callErr = c.providers[pool.Provider].Create(ctx, provider.CreateRequest{OperationID: slot.Operation.ID, Options: pool.Options, Runner: c.spec(pool, index)})
		return callErr
	})
	if err != nil {
		var remote *provider.Error
		uncertain := !errors.As(err, &remote) || !remote.NoSideEffect || remote.Kind == provider.Uncertain
		if !uncertain {
			if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) { slot.Operation = nil }); err != nil {
				return err
			}
		}
		return c.failure(pool.Name, index, err, uncertain)
	}
	if resource.ID == "" {
		return c.failure(pool.Name, index, errors.New("creation omitted resource ID"), true)
	}
	return c.store.Update(func(saved *state.State) error {
		p := saved.Pools[pool.Name]
		slot := &p.Slots[index]
		slot.ResourceID = resource.ID
		slot.Power = provider.Unknown
		slot.Operation.Kind = "await-running"
		slot.Operation.Phase = "requested"
		slot.ChangedAt = c.now()
		slot.LastError = ""
		associateProbeHost(saved, p, pool.Name, index, resource.ID, slot.Operation.ProbeKey)
		slot.Operation.ProbeKey = ""
		saved.Pools[pool.Name] = p
		return nil
	})
}

func (c *Controller) releaseKind(pool config.Pool) string {
	if c.cfg.Providers[pool.Provider].Lifecycle == provider.Disposable {
		return "destroy"
	}
	return "stop"
}

func (c *Controller) finishRelease(ctx context.Context, pool config.Pool, index int) error {
	slot := c.store.Snapshot().Pools[pool.Name].Slots[index]
	if slot.Operation == nil || (slot.Operation.Kind != "stop" && slot.Operation.Kind != "destroy") {
		return nil
	}
	if slot.Operation.Phase == "requested" && c.now().Sub(slot.ChangedAt) < time.Duration(c.cfg.ObservationGrace) {
		return nil
	}
	if !c.providerVerified(c.store.Snapshot(), pool.Provider) {
		return state.ErrStale
	}
	if slot.Operation.Phase == "" {
		drained := slot.Power == provider.Stopped && slot.Operation.Kind == "destroy"
		if !drained {
			err := c.mutate(ctx, pool.Provider, func(ctx context.Context) error {
				var callErr error
				drained, callErr = c.providers[pool.Provider].Drain(ctx, slot.ResourceID, pool.Options, c.spec(pool, index).Name)
				return callErr
			})
			if err != nil {
				return c.failure(pool.Name, index, err, false)
			}
		}
		if !drained {
			return nil
		}
		if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) { slot.Operation.Phase = "drained" }); err != nil {
			return err
		}
	}
	if !c.providerVerified(c.store.Snapshot(), pool.Provider) {
		return state.ErrStale
	}
	if err := c.slotUpdate(pool.Name, index, func(slot *state.Slot) { slot.Operation.Phase = "requested"; slot.ChangedAt = c.now() }); err != nil {
		return err
	}
	err := c.mutate(ctx, pool.Provider, func(ctx context.Context) error {
		if slot.Operation.Kind == "destroy" {
			return c.providers[pool.Provider].Destroy(ctx, slot.ResourceID)
		}
		return c.providers[pool.Provider].Stop(ctx, slot.ResourceID)
	})
	if err != nil {
		var remote *provider.Error
		if errors.As(err, &remote) && remote.NoSideEffect && remote.Operation == "dispatch" {
			if saveErr := c.slotUpdate(pool.Name, index, func(slot *state.Slot) { slot.Operation.Phase = "drained" }); saveErr != nil {
				return saveErr
			}
		}
		return c.failure(pool.Name, index, err, false)
	}
	return nil
}

func (c *Controller) reclaim(ctx context.Context, providerName string, demands map[string]demand) error {
	if !c.providerVerified(c.store.Snapshot(), providerName) {
		return nil
	}
	pools := orderedPools(c.cfg.Pools)
	slices.Reverse(pools)
	for _, pool := range pools {
		if pool.Provider != providerName || !demands[pool.Name].idle() {
			continue
		}
		for index, slot := range c.store.Snapshot().Pools[pool.Name].Slots {
			if slot.Power != provider.Running || slot.Operation != nil || c.now().Before(slot.RetryAt) {
				continue
			}
			if err := c.begin(pool.Name, index, c.releaseKind(pool)); err != nil {
				return err
			}
			return c.finishRelease(ctx, pool, index)
		}
	}
	return nil
}

func (c *Controller) ResolveCreate(poolName string, index int, resourceID string, confirmAbsent bool) error {
	return c.store.Update(func(saved *state.State) error {
		p, exists := saved.Pools[poolName]
		if !exists || index < 0 || index >= len(p.Slots) {
			return errors.New("unknown pool or slot")
		}
		slot := &p.Slots[index]
		if slot.Operation == nil || (slot.Operation.Kind != "create" && slot.Operation.Kind != "unresolved") {
			return errors.New("slot has no unresolved creation")
		}
		if (resourceID == "") == !confirmAbsent {
			return errors.New("provide exactly one of resource ID or explicit absence confirmation")
		}
		probeKey := slot.Operation.ProbeKey
		slot.ResourceID, slot.Operation, slot.LastError, slot.RetryAt = resourceID, nil, "", time.Time{}
		slot.Power = provider.Stopped
		if resourceID != "" {
			slot.Power = provider.Unknown
			associateProbeHost(saved, p, poolName, index, resourceID, probeKey)
		}
		p.Slots[index] = *slot
		saved.Pools[poolName] = p
		return nil
	})
}
