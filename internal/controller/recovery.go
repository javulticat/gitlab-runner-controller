package controller

import (
	"context"
	"errors"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func audit(saved *state.State, action, pool string, index int, id string, binding provider.Description) {
	saved.Audit = append(saved.Audit, state.AuditRecord{ID: operationID(), At: time.Now().UTC(), Action: action, Pool: pool, Slot: index, ResourceID: id, Binding: binding})
	if len(saved.Audit) > 1000 {
		saved.Audit = saved.Audit[len(saved.Audit)-1000:]
	}
}

// ReleaseMissing is an offline audit. It never asks a provider to change a resource.
func ReleaseMissing(ctx context.Context, cfg config.Config, store *state.Store, providers map[string]provider.Provider, poolName string, index int, expectedID string, confirmAbsent bool) (string, error) {
	if !confirmAbsent || expectedID == "" {
		return "", errors.New("release-missing requires an exact --resource-id and --confirm-absent after an account audit")
	}
	before := store.Snapshot()
	pool, found := before.Pools[poolName]
	if !found || index < 0 || index >= len(pool.Slots) {
		return "", errors.New("unknown pool or slot")
	}
	slot := pool.Slots[index]
	if slot.ResourceID == "" || slot.ResourceID != expectedID {
		return "", errors.New("resource ID does not match the saved slot")
	}
	binding, bound := before.ProviderBindings[pool.Provider]
	spec, configured := cfg.Providers[pool.Provider]
	instance := providers[pool.Provider]
	if !bound || !configured || instance == nil || spec.Identity != binding.Identity || spec.Lifecycle != binding.Lifecycle {
		return "", errors.New("restore the saved provider mapping before releasing a missing resource")
	}
	description, err := instance.Describe(ctx)
	if err != nil {
		return "", err
	}
	if !description.Valid() || description.Identity != binding.Identity || description.Scope != binding.Scope || description.Lifecycle != binding.Lifecycle || description.AbsenceConfirmsDestroy != binding.AbsenceConfirmsDestroy {
		return "", errors.New("provider binding differs from the saved resource")
	}
	resources, err := instance.List(ctx)
	if err != nil {
		return "", err
	}
	inventory, err := indexInventory(resources)
	if err != nil {
		return "", err
	}
	if _, present := inventory[expectedID]; present {
		return "", errors.New("resource is still present; release-missing cannot discard a running or stopped host")
	}
	backup, err := store.Backup()
	if err != nil {
		return backup, err
	}
	err = store.Update(func(saved *state.State) error {
		if saved.Revision != before.Revision {
			return errors.New("state changed during the missing-resource audit")
		}
		p := saved.Pools[poolName]
		if p.Slots[index].ResourceID != expectedID {
			return errors.New("saved resource changed during the audit")
		}
		releaseResourceProbes(saved, p.RunnerID, expectedID, time.Now(), false)
		p.Slots[index] = state.Slot{Power: provider.Stopped}
		saved.Pools[poolName] = p
		audit(saved, "release-missing", poolName, index, expectedID, binding)
		return nil
	})
	return backup, err
}

// RetryPending resets only an exhausted attempt, leaving assignment to GitLab.
func RetryPending(cfg config.Config, store *state.Store, poolName string, projectID, jobID int64, confirmRetry bool) (string, error) {
	if !confirmRetry {
		return "", errRetryConfirmation
	}
	var pool config.Pool
	for _, p := range cfg.Pools {
		if p.Name == poolName {
			pool = p
			break
		}
	}
	if pool.Name == "" {
		return "", errors.New("unknown configured pool")
	}
	before := store.Snapshot()
	key := state.PendingProbeKey(pool.RunnerID, projectID, jobID)
	probe, exists := before.PendingProbes[key]
	job, present := before.Jobs[state.JobKey(projectID, jobID)]
	if !exists || !probe.Exhausted || !present || job.Status != "pending" || probe.RunnerID != pool.RunnerID {
		return "", errors.New("no exhausted pending attempt exists for this runner and job")
	}
	p, known := before.Pools[poolName]
	binding, bound := before.ProviderBindings[pool.Provider]
	if !known || p.RunnerID != pool.RunnerID || p.Provider != pool.Provider || !bound || binding.Identity != cfg.Providers[pool.Provider].Identity || binding.Lifecycle != cfg.Providers[pool.Provider].Lifecycle {
		return "", errors.New("pool and provider mapping differ from saved ownership")
	}
	backup, err := store.Backup()
	if err != nil {
		return backup, err
	}
	err = store.Update(func(saved *state.State) error {
		if saved.Revision != before.Revision {
			return errors.New("state changed during pending-job audit")
		}
		delete(saved.PendingProbes, key)
		audit(saved, "retry-pending", poolName, probe.Slot, probe.ResourceID, binding)
		return nil
	})
	return backup, err
}
