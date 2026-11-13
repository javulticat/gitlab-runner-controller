package controller

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/gitlab"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type JobsAPI interface {
	Sync(context.Context, []int64, []state.Job) ([]state.Job, error)
}

type runnerPolicyAPI interface {
	Policies(context.Context, []int64, []int64) (map[int64]gitlab.RunnerPolicy, error)
}

var errUnverifiedDispatch = &provider.Error{Operation: "dispatch", Kind: provider.Transient, Detail: "job observations await synchronization", NoSideEffect: true}

type Controller struct {
	cfg            config.Config
	store          *state.Store
	jobs           JobsAPI
	providers      map[string]provider.Provider
	log            *slog.Logger
	wake           chan struct{}
	stepMu         sync.Mutex
	now            func() time.Time
	first          bool
	statusMu       sync.RWMutex
	lastError      string
	lastStep       time.Time
	metrics        *Metrics
	stopping       atomic.Bool
	runnerPolicies map[int64]gitlab.RunnerPolicy
}

func Binding(cfg config.Config) string {
	projects := slices.Clone(cfg.ProjectIDs)
	slices.Sort(projects)
	data, _ := json.Marshal(struct {
		URL      string
		Projects []int64
	}{cfg.GitLabURL, projects})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func New(cfg config.Config, store *state.Store, jobs JobsAPI, providers map[string]provider.Provider, logger *slog.Logger) (*Controller, error) {
	c := &Controller{cfg: cfg, store: store, jobs: jobs, providers: providers, log: logger, wake: make(chan struct{}, 1), now: time.Now, first: true, metrics: NewMetrics()}
	for name := range cfg.Providers {
		if providers[name] == nil {
			return nil, fmt.Errorf("provider %s is not registered", name)
		}
	}
	for _, pool := range cfg.Pools {
		if err := providers[pool.Provider].ValidateOptions(pool.Options); err != nil {
			return nil, fmt.Errorf("pool %s options: %w", pool.Name, err)
		}
	}
	err := store.Update(func(saved *state.State) error {
		configured := map[string]bool{}
		for _, pool := range cfg.Pools {
			configured[pool.Name] = true
			p, exists := saved.Pools[pool.Name]
			if exists && p.Provider != pool.Provider && p.HasHandles() {
				return fmt.Errorf("pool %s provider cannot change while saved resources exist", pool.Name)
			}
			p.Provider = pool.Provider
			if p.RunnerID != 0 && p.RunnerID != pool.RunnerID && p.HasHandles() {
				return fmt.Errorf("pool %s runner_id cannot change while saved handles exist", pool.Name)
			}
			p.RunnerID = pool.RunnerID
			optionsHash := OptionsHash(pool.Options)
			if exists && p.OptionsHash != optionsHash && p.HasHandles() {
				return fmt.Errorf("pool %s compute options cannot change while saved handles exist", pool.Name)
			}
			p.OptionsHash = optionsHash
			for len(p.Slots) < pool.MaxInstances {
				p.Slots = append(p.Slots, state.Slot{Power: provider.Stopped})
			}
			for i, id := range pool.AdoptIDs {
				if slices.Contains(p.AdoptedIDs, id) {
					continue
				}
				if p.Slots[i].ResourceID != "" && p.Slots[i].ResourceID != id {
					return fmt.Errorf("pool %s adoption conflicts with saved slot %d", pool.Name, i)
				}
				if p.Slots[i].Operation != nil && p.Slots[i].Operation.Kind == "unresolved" {
					return errors.New("use resolve-create to bind an unresolved creation")
				}
			}
			for i, slot := range p.Slots {
				if slot.Operation != nil && slot.Operation.Kind == "create" {
					p.Slots[i].Operation.Kind = "unresolved"
					p.Slots[i].LastError = "creation was interrupted; resolve the operation before retrying"
				}
			}
			saved.Pools[pool.Name] = p
		}
		for name, pool := range saved.Pools {
			if configured[name] {
				continue
			}
			for _, slot := range pool.Slots {
				if slot.Power != provider.Stopped || slot.Operation != nil || (saved.ProviderBindings[pool.Provider].Lifecycle == provider.Disposable && slot.ResourceID != "") {
					return fmt.Errorf("removed pool %s still has active or unresolved resources", name)
				}
			}
			if !pool.HasHandles() {
				delete(saved.Pools, name)
			}
		}
		for name := range saved.ProviderBindings {
			if _, configured := cfg.Providers[name]; !configured && !saved.HasHandles(name) {
				delete(saved.ProviderBindings, name)
				delete(saved.ProviderBackoff, name)
				delete(saved.ProviderErrors, name)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Controller) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Controller) Hint(hint state.Hint) error {
	key := state.JobKey(hint.ProjectID, hint.ID)
	if !slices.Contains(c.cfg.ProjectIDs, hint.ProjectID) || hint.ID <= 0 {
		return errors.New("invalid job hint")
	}
	hint.Received = c.now()
	changed := false
	err := c.store.Update(func(saved *state.State) error {
		if old, ok := saved.Hints[key]; ok && old.Status == hint.Status {
			return state.ErrNoChange
		}
		if job, ok := saved.Jobs[key]; ok && job.Status == hint.Status {
			if _, dirty := saved.Hints[key]; !dirty {
				return state.ErrNoChange
			}
		}
		if len(saved.Hints) >= 10000 {
			return errors.New("job hint inbox is full")
		}
		saved.Hints[key] = hint
		saved.HintRevision++
		changed = true
		return nil
	})
	if err == nil && changed {
		c.Wake()
	}
	return err
}

func (c *Controller) Step(ctx context.Context) (err error) {
	c.stepMu.Lock()
	defer c.stepMu.Unlock()
	started := c.now()
	deferred := false
	defer func() {
		if deferred {
			c.metrics.state.observe(c.store.Snapshot(), c.now())
			return
		}
		c.statusMu.Lock()
		c.lastStep = c.now()
		c.lastError = ""
		if err != nil {
			c.lastError = err.Error()
		}
		c.statusMu.Unlock()
		c.metrics.ObserveAt(c.store.Snapshot(), c.now(), c.now().Sub(started), err)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.admissionStopped(ctx) {
		return ErrStopping
	}
	proceed, err := c.refresh(ctx)
	if err != nil {
		return err
	}
	if !proceed || !c.verified(c.store.Snapshot()) {
		deferred = true
		return nil
	}
	inventory := map[string]map[string]provider.Resource{}
	descriptions := map[string]provider.Description{}
	var failures []error
	names := make([]string, 0, len(c.providers))
	for name := range c.providers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if c.admissionStopped(ctx) {
			return ErrStopping
		}
		if c.now().Before(c.store.Snapshot().ProviderBackoff[name]) {
			failures = append(failures, fmt.Errorf("provider %s is in backoff", name))
			continue
		}
		description, err := c.providers[name].Describe(ctx)
		if err != nil {
			failures = append(failures, c.providerFailure(name, err))
			continue
		}
		descriptions[name] = description
	}
	duplicates := map[string]bool{}
	accounts := map[string]string{}
	for _, name := range names {
		if description, ok := descriptions[name]; ok {
			if previous, duplicate := accounts[description.AccountKey()]; duplicate {
				duplicates[name], duplicates[previous] = true, true
			}
			accounts[description.AccountKey()] = name
		}
	}
	for _, name := range names {
		if c.admissionStopped(ctx) {
			return ErrStopping
		}
		description, ok := descriptions[name]
		if !ok {
			continue
		}
		if duplicates[name] {
			failures = append(failures, c.providerFailure(name, errors.New("duplicate provider aliases target one account and scope")))
			continue
		}
		if err := c.bind(name, description); err != nil {
			failures = append(failures, c.providerFailure(name, err))
			continue
		}
		resources, err := c.providers[name].List(ctx)
		if err != nil {
			failures = append(failures, c.providerFailure(name, err))
			continue
		}
		index, err := indexInventory(resources)
		if err != nil {
			failures = append(failures, c.providerFailure(name, err))
			continue
		}
		inventory[name] = index
		if err := c.store.Update(func(saved *state.State) error { delete(saved.ProviderErrors, name); return nil }); err != nil {
			return err
		}
	}
	for _, name := range names {
		resources, available := inventory[name]
		if !available {
			continue
		}
		needed := false
		for _, pool := range c.cfg.Pools {
			if pool.Provider == name && len(pool.AdoptIDs) > 0 {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		if err := c.adopt(map[string]map[string]provider.Resource{name: resources}); err != nil {
			failures = append(failures, c.providerFailure(name, err))
			delete(inventory, name)
		}
	}
	if err := c.observe(inventory); err != nil {
		return err
	}
	scheduleErr := c.schedule(ctx, inventory)
	if scheduleErr == state.ErrStale {
		deferred = len(failures) == 0
		scheduleErr = nil
	}
	return errors.Join(append(failures, scheduleErr)...)
}

func (c *Controller) verified(saved state.State) bool {
	return !saved.LastSync.IsZero() && saved.HintRevision == saved.VerifiedRevision && !c.now().Before(saved.BackoffUntil)
}

func (c *Controller) providerVerified(saved state.State, name string) bool {
	return !c.stopping.Load() && c.verified(saved) && !c.now().Before(saved.ProviderBackoff[name]) && saved.ProviderErrors[name] == ""
}

func (c *Controller) mutate(ctx context.Context, name string, call func(context.Context) error) error {
	if c.admissionStopped(ctx) {
		return errors.Join(ErrStopping, &provider.Error{Operation: "dispatch", Kind: provider.Transient, Detail: "controller is stopping", NoSideEffect: true})
	}
	saved := c.store.Snapshot()
	if !c.verified(saved) {
		return errUnverifiedDispatch
	}
	if !c.providerVerified(saved, name) {
		return &provider.Error{Operation: "dispatch", Kind: provider.Transient, Detail: "job or account observations are not verified", NoSideEffect: true}
	}
	if _, running := ctx.Value(admissionContextKey{}).(context.Context); running {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < time.Duration(c.cfg.RequestTimeout) {
			return &provider.Error{Operation: "dispatch", Kind: provider.Transient, Detail: "reconciliation has insufficient time for another bounded request", NoSideEffect: true}
		}
	}
	return call(ctx)
}

func (c *Controller) refresh(ctx context.Context) (bool, error) {
	snapshot := c.store.Snapshot()
	now := c.now()
	if now.Before(snapshot.BackoffUntil) {
		return false, errors.New("GitLab synchronization is in backoff")
	}
	dirty := snapshot.HintRevision != snapshot.VerifiedRevision
	if !c.first && !dirty && now.Sub(snapshot.LastSync) < time.Duration(c.cfg.JobsPollInterval) {
		return true, nil
	}
	if now.Before(snapshot.NextRefresh) {
		return false, nil
	}
	if err := c.store.Update(func(saved *state.State) error {
		saved.NextRefresh = now.Add(time.Duration(c.cfg.MinRefresh))
		return nil
	}); err != nil {
		return false, err
	}
	snapshot = c.store.Snapshot()
	check := []state.Job{}
	for _, job := range snapshot.Jobs {
		if state.Active(job.Status) {
			check = append(check, job)
		}
	}
	for _, hint := range snapshot.Hints {
		check = append(check, state.Job{ProjectID: hint.ProjectID, ID: hint.ID})
	}
	jobs, err := c.jobs.Sync(ctx, c.cfg.ProjectIDs, check)
	var policies map[int64]gitlab.RunnerPolicy
	if err == nil {
		if c.admissionStopped(ctx) {
			return false, ErrStopping
		}
		if reader, ok := c.jobs.(runnerPolicyAPI); ok {
			ids := make([]int64, 0, len(c.cfg.Pools))
			for _, pool := range c.cfg.Pools {
				ids = append(ids, pool.RunnerID)
			}
			policies, err = reader.Policies(ctx, c.cfg.ProjectIDs, ids)
		}
	}
	if err != nil {
		var limited *gitlab.RateLimitError
		delay := time.Duration(30*(1<<min(snapshot.BackoffFailures, 5))) * time.Second
		if errors.As(err, &limited) && limited.Delay > 0 {
			delay = limited.Delay
		} else {
			delay += time.Duration(mathrand.Int64N(int64(delay/5) + 1))
		}
		if saveErr := c.store.Update(func(saved *state.State) error {
			saved.BackoffUntil = c.now().Add(delay)
			saved.BackoffFailures = min(saved.BackoffFailures+1, 10)
			return nil
		}); saveErr != nil {
			return false, errors.Join(err, saveErr)
		}
		return false, err
	}
	err = c.store.Update(func(saved *state.State) error {
		if saved.HintRevision != snapshot.HintRevision {
			return state.ErrStale
		}
		for key, job := range saved.Jobs {
			if state.Active(job.Status) || now.Sub(job.UpdatedAt) > time.Hour {
				delete(saved.Jobs, key)
			}
		}
		for _, job := range jobs {
			if job.ID <= 0 || !slices.Contains(c.cfg.ProjectIDs, job.ProjectID) {
				return errors.New("GitLab returned a job outside configured projects")
			}
			job.UpdatedAt = now
			saved.Jobs[job.Key()] = job
		}
		for _, pool := range c.cfg.Pools {
			p := saved.Pools[pool.Name]
			p.RunnerError = policyError(pool, policies[pool.RunnerID])
			if p.RunnerError == "" {
				for _, project := range c.cfg.ProjectIDs {
					if _, known := policies[pool.RunnerID].Projects[project]; !known {
						p.RunnerError = "runner availability is unknown for a configured project"
						if policies[pool.RunnerID].Error != "" {
							p.RunnerError = policies[pool.RunnerID].Error
						}
						break
					}
				}
			}
			saved.Pools[pool.Name] = p
		}
		for key, probe := range saved.PendingProbes {
			current, found := saved.Jobs[state.JobKey(probe.ProjectID, probe.JobID)]
			if !found || current.Status != "pending" {
				delete(saved.PendingProbes, key)
			}
		}
		saved.LastSync = now
		saved.VerifiedRevision = saved.HintRevision
		saved.Hints = map[string]state.Hint{}
		saved.BackoffUntil = time.Time{}
		saved.BackoffFailures = 0
		return nil
	})
	if err == nil {
		c.first = false
		c.runnerPolicies = policies
	}
	if err == state.ErrStale {
		return false, nil
	}
	return err == nil, err
}

func (c *Controller) observe(inventory map[string]map[string]provider.Resource) error {
	now := c.now()
	return c.store.Update(func(saved *state.State) error {
		verified := c.verified(*saved)
		var demands map[string]demand
		if verified {
			demands = c.demands(*saved)
		}
		for name, until := range saved.ProviderBackoff {
			if !now.Before(until) {
				delete(saved.ProviderBackoff, name)
			}
		}
		for name, pool := range saved.Pools {
			resources, configured := inventory[pool.Provider]
			if !configured {
				continue
			}
			idle := verified && demands[name].idle()
			for i, slot := range pool.Slots {
				if slot.ResourceID == "" {
					continue
				}
				resource, found := resources[slot.ResourceID]
				if found && resource.State != provider.Unknown {
					slot.MissingSince = time.Time{}
				}
				if !found && slot.Operation != nil && slot.Operation.Kind == "destroy" && slot.Operation.Phase == "requested" && saved.ProviderBindings[pool.Provider].AbsenceConfirmsDestroy {
					releaseResourceProbes(saved, pool.RunnerID, slot.ResourceID, now, c.otherProbeOpportunity(*saved, pool.RunnerID, slot.ResourceID))
					slot = state.Slot{Power: provider.Stopped}
				} else if !found || resource.State == provider.Unknown {
					if slot.MissingSince.IsZero() {
						slot.MissingSince = now
					}
					slot.Power, slot.Ready = provider.Unknown, false
					slot.LastError = "resource is missing or unknown; capacity remains reserved"
				} else if slot.Operation != nil && slot.Operation.Kind == "destroy" {
					slot.Power = resource.State
				} else if slot.Operation != nil && slot.Operation.Kind == "stop" {
					if resource.State == provider.Stopped {
						slot.Power, slot.Ready, slot.Operation = provider.Stopped, false, nil
						slot.LastError = ""
					}
				} else if slot.Operation != nil && (slot.Operation.Kind == "resume" || slot.Operation.Kind == "await-running") {
					if resource.State == provider.Running {
						slot.Power, slot.Operation = provider.Running, nil
						slot.RetryAt = time.Time{}
					} else if slot.Operation.Kind == "await-running" && bootstrapExpired(slot, now) {
						slot.Power = provider.Stopped
						if saved.ProviderBindings[pool.Provider].Lifecycle == provider.Disposable {
							slot.Operation = nil
						} else {
							slot.Operation.Kind, slot.Operation.Phase = "resume", ""
						}
					}
				} else {
					slot.MissingSince = time.Time{}
					if slot.Power != resource.State {
						slot.ChangedAt = now
					}
					slot.Power = resource.State
					if resource.State == provider.Stopped {
						slot.Ready = false
					}
					if slot.LastError == "resource is missing or unknown; capacity remains reserved" {
						slot.LastError = ""
					}
				}
				if resource.State == provider.Stopped && saved.ProviderBindings[pool.Provider].Lifecycle == provider.Retained &&
					slot.Operation != nil && slot.Operation.Kind == "resume" && idle &&
					!slot.ChangedAt.IsZero() && now.Sub(slot.ChangedAt) >= time.Duration(c.cfg.ObservationGrace) {
					slot.Power, slot.Ready, slot.Operation = provider.Stopped, false, nil
					slot.BootstrapStartedAt, slot.BootstrapDeadline = time.Time{}, time.Time{}
					slot.LastError, slot.RetryAt = "", time.Time{}
					slot.ChangedAt = now
				}
				// RetryAt records whether the first timeout backoff has already started.
				if resource.State == provider.Stopped && saved.ProviderBindings[pool.Provider].Lifecycle == provider.Retained && bootstrapExpired(slot, now) &&
					(slot.Operation == nil || slot.Operation.Kind == "resume") && (slot.LastError == "" || slot.RetryAt.Before(slot.BootstrapDeadline.Add(c.bootstrapBackoff()))) {
					c.deferBootstrap(&slot, now)
				}
				pool.Slots[i] = slot
			}
			saved.Pools[name] = pool
		}
		return nil
	})
}

func (c *Controller) spec(pool config.Pool, index int) provider.RunnerSpec {
	spec := provider.RunnerSpec{Name: fmt.Sprintf("%s/%s/%d", c.cfg.ControllerID, pool.Name, index), GitLabURL: c.cfg.GitLabURL,
		Token: pool.Token, Jobs: pool.JobsPerInstance, Options: c.cfg.Runtime}
	data, _ := json.Marshal(struct {
		Runner     provider.RunnerSpec
		Compute    json.RawMessage
		Deployment string
	}{spec, pool.Options, c.cfg.Providers[pool.Provider].Revision})
	hash := sha256.Sum256(data)
	spec.Revision = hex.EncodeToString(hash[:])
	return spec
}

func OptionsHash(raw json.RawMessage) string {
	var options any
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if json.Unmarshal(raw, &options) != nil {
		return ""
	}
	canonical, _ := json.Marshal(options)
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:])
}

func (c *Controller) bind(name string, description provider.Description) error {
	spec := c.cfg.Providers[name]
	if !description.Valid() || description.Identity != spec.Identity || description.Lifecycle != spec.Lifecycle {
		return errors.New("provider identity or lifecycle does not match configuration")
	}
	return c.store.Update(func(saved *state.State) error {
		old, exists := saved.ProviderBindings[name]
		if exists && (old.Identity != description.Identity || old.Scope != description.Scope || old.Lifecycle != description.Lifecycle || old.AbsenceConfirmsDestroy != description.AbsenceConfirmsDestroy) && saved.HasHandles(name) {
			return errors.New("provider binding changed while saved handles exist; explicit migration required")
		}
		for key, bound := range saved.ProviderBindings {
			if key != name && bound.Identity == description.Identity && bound.Scope == description.Scope {
				return errors.New("provider aliases cannot share one account and scope")
			}
		}
		saved.ProviderBindings[name] = description
		return nil
	})
}

func (c *Controller) adopt(inventory map[string]map[string]provider.Resource) error {
	return c.store.Update(func(saved *state.State) error {
		for _, spec := range c.cfg.Pools {
			if _, available := inventory[spec.Provider]; !available {
				continue
			}
			pool := saved.Pools[spec.Name]
			for index, id := range spec.AdoptIDs {
				if slices.Contains(pool.AdoptedIDs, id) {
					continue
				}
				if pool.Slots[index].ResourceID == id {
					pool.AdoptedIDs = append(pool.AdoptedIDs, id)
					continue
				}
				if pool.Slots[index].ResourceID != "" || pool.Slots[index].Operation != nil {
					return errors.New("adoption would replace an existing handle or operation")
				}
				resource, found := inventory[spec.Provider][id]
				if !found || resource.State == provider.Unknown {
					return errors.New("adoption resource is not present in verified provider inventory")
				}
				pool.Slots[index] = state.Slot{ResourceID: id, Power: resource.State}
				pool.AdoptedIDs = append(pool.AdoptedIDs, id)
			}
			saved.Pools[spec.Name] = pool
		}
		return nil
	})
}

func (c *Controller) providerFailure(name string, err error) error {
	var remote *provider.Error
	if saveErr := c.store.Update(func(saved *state.State) error {
		if saved.ProviderErrors == nil {
			saved.ProviderErrors = map[string]string{}
		}
		saved.ProviderErrors[name] = err.Error()
		if errors.As(err, &remote) && remote.RetryAfter > 0 {
			saved.ProviderBackoff[name] = c.now().Add(remote.RetryAfter)
		}
		return nil
	}); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return fmt.Errorf("provider %s: %w", name, err)
}

func operationID() string {
	return cryptorand.Text()
}

func orderedPools(pools []config.Pool) []config.Pool {
	ordered := slices.Clone(pools)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority > ordered[j].Priority })
	return ordered
}
