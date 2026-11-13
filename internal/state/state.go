package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

const Version = 1

var ErrStale = errors.New("new job hints arrived during synchronization")

var ErrNoChange = errors.New("transaction has no changes")

type Operation struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Phase     string    `json:"phase,omitempty"`
	ProbeKey  string    `json:"probe_key,omitempty"`
}

type Slot struct {
	ResourceID         string              `json:"resource_id,omitempty"`
	Power              provider.PowerState `json:"power"`
	Persisted          bool                `json:"persisted"`
	Ready              bool                `json:"ready"`
	RuntimeRevision    string              `json:"runtime_revision,omitempty"`
	Operation          *Operation          `json:"operation,omitempty"`
	ChangedAt          time.Time           `json:"changed_at,omitempty"`
	RetryAt            time.Time           `json:"retry_at,omitempty"`
	LastError          string              `json:"last_error,omitempty"`
	MissingSince       time.Time           `json:"missing_since,omitempty"`
	BootstrapStartedAt time.Time           `json:"bootstrap_started_at,omitempty"`
	BootstrapDeadline  time.Time           `json:"bootstrap_deadline,omitempty"`
	ConfiguredJobs     int                 `json:"configured_jobs,omitempty"`
}

type Pool struct {
	Provider    string    `json:"provider"`
	OptionsHash string    `json:"options_hash"`
	AdoptedIDs  []string  `json:"adopted_ids,omitempty"`
	IdleSince   time.Time `json:"idle_since,omitempty"`
	Slots       []Slot    `json:"slots"`
	RunnerID    int64     `json:"runner_id,omitempty"`
	RunnerError string    `json:"runner_error,omitempty"`
}

type Job struct {
	ProjectID int64     `json:"project_id"`
	ID        int64     `json:"id"`
	Status    string    `json:"status"`
	Tags      []string  `json:"tags"`
	UpdatedAt time.Time `json:"updated_at"`
	RunnerID  int64     `json:"runner_id,omitempty"`
}

func (j Job) Key() string { return JobKey(j.ProjectID, j.ID) }

func JobKey(project, job int64) string {
	return strconv.FormatInt(project, 10) + "/" + strconv.FormatInt(job, 10)
}

func Active(status string) bool {
	return slices.Contains([]string{"pending", "preparing", "running", "canceling"}, status)
}

type Hint struct {
	ProjectID int64     `json:"project_id"`
	ID        int64     `json:"id"`
	Status    string    `json:"status"`
	Received  time.Time `json:"received"`
}

type State struct {
	Version          int                             `json:"version"`
	ControllerID     string                          `json:"controller_id"`
	Binding          string                          `json:"binding"`
	Revision         uint64                          `json:"revision"`
	HintRevision     uint64                          `json:"hint_revision"`
	VerifiedRevision uint64                          `json:"verified_revision"`
	LastSync         time.Time                       `json:"last_sync,omitempty"`
	NextRefresh      time.Time                       `json:"next_refresh,omitempty"`
	BackoffUntil     time.Time                       `json:"backoff_until,omitempty"`
	BackoffFailures  int                             `json:"backoff_failures"`
	ProviderBackoff  map[string]time.Time            `json:"provider_backoff"`
	ProviderBindings map[string]provider.Description `json:"provider_bindings"`
	Pools            map[string]Pool                 `json:"pools"`
	Jobs             map[string]Job                  `json:"jobs"`
	Hints            map[string]Hint                 `json:"hints"`
	ProviderErrors   map[string]string               `json:"provider_errors,omitempty"`
	Audit            []AuditRecord                   `json:"audit,omitempty"`
	PendingProbes    map[string]PendingProbe         `json:"pending_probes,omitempty"`
}

func New(controllerID, binding string) State {
	return State{Version: Version, ControllerID: controllerID, Binding: binding,
		Pools: map[string]Pool{}, Jobs: map[string]Job{}, Hints: map[string]Hint{}, ProviderBackoff: map[string]time.Time{}, ProviderBindings: map[string]provider.Description{}}
}

func (s State) Clone() State {
	s.Pools = maps.Clone(s.Pools)
	for key, p := range s.Pools {
		p.Slots = slices.Clone(p.Slots)
		p.AdoptedIDs = slices.Clone(p.AdoptedIDs)
		for i, slot := range p.Slots {
			if slot.Operation != nil {
				op := *slot.Operation
				p.Slots[i].Operation = &op
			}
		}
		s.Pools[key] = p
	}
	s.Jobs = maps.Clone(s.Jobs)
	for key, job := range s.Jobs {
		job.Tags = slices.Clone(job.Tags)
		s.Jobs[key] = job
	}
	s.Hints = maps.Clone(s.Hints)
	s.ProviderBackoff = maps.Clone(s.ProviderBackoff)
	s.ProviderBindings = maps.Clone(s.ProviderBindings)
	s.ProviderErrors = maps.Clone(s.ProviderErrors)
	s.Audit = slices.Clone(s.Audit)
	s.PendingProbes = maps.Clone(s.PendingProbes)
	return s
}

type AuditRecord struct {
	ID         string               `json:"id"`
	At         time.Time            `json:"at"`
	Action     string               `json:"action"`
	Pool       string               `json:"pool"`
	Slot       int                  `json:"slot"`
	ResourceID string               `json:"resource_id"`
	Binding    provider.Description `json:"binding"`
}

type PendingProbe struct {
	ProjectID        int64         `json:"project_id"`
	JobID            int64         `json:"job_id"`
	Pool             string        `json:"pool"`
	Slot             int           `json:"slot"`
	ReservedAt       time.Time     `json:"reserved_at"`
	Deadline         time.Time     `json:"deadline,omitempty"`
	Exhausted        bool          `json:"exhausted"`
	RunnerID         int64         `json:"runner_id"`
	ResourceID       string        `json:"resource_id,omitempty"`
	Remaining        time.Duration `json:"remaining,omitempty"`
	HostAttempts     int           `json:"host_attempts,omitempty"`
	ExhaustionReason string        `json:"exhaustion_reason,omitempty"`
}

func PendingProbeKey(runnerID, project, job int64) string {
	return JobKey(project, job) + "/runner-" + strconv.FormatInt(runnerID, 10)
}

func (s State) Validate() error {
	if s.Version != Version || s.ControllerID == "" || s.Binding == "" || s.Pools == nil || s.Jobs == nil || s.Hints == nil || s.ProviderBackoff == nil || s.ProviderBindings == nil {
		return errors.New("invalid or unsupported state schema; preserve the file for recovery")
	}
	if s.VerifiedRevision > s.HintRevision {
		return errors.New("invalid verified job revision")
	}
	ids := map[string]bool{}
	accounts := map[string]bool{}
	for _, binding := range s.ProviderBindings {
		if !binding.Valid() {
			return errors.New("invalid provider binding")
		}
		key := binding.AccountKey()
		if accounts[key] {
			return errors.New("provider aliases cannot share one account and scope")
		}
		accounts[key] = true
	}
	for name, p := range s.Pools {
		if name == "" || p.Provider == "" || p.OptionsHash == "" || p.RunnerID <= 0 || len(p.Slots) == 0 || len(p.Slots) > 1000 {
			return errors.New("invalid saved pool")
		}
		for _, slot := range p.Slots {
			if slot.ConfiguredJobs < 0 || slot.ConfiguredJobs > 64 {
				return errors.New("invalid configured runner concurrency")
			}
			if slot.Ready && (slot.ResourceID == "" || slot.Power != provider.Running || slot.Operation != nil) {
				return errors.New("ready runner has no active resource")
			}
			if slot.Power == provider.Running && slot.ResourceID == "" {
				return errors.New("running resource omitted its identity")
			}
			if !slot.BootstrapDeadline.IsZero() && (slot.BootstrapStartedAt.IsZero() || slot.BootstrapDeadline.Before(slot.BootstrapStartedAt)) {
				return errors.New("invalid bootstrap readiness deadline")
			}
			binding, bound := s.ProviderBindings[p.Provider]
			if (slot.ResourceID != "" || slot.Operation != nil) && !bound {
				return errors.New("saved resource has no verified provider binding")
			}
			if slot.ResourceID != "" {
				key := p.Provider + "/" + slot.ResourceID
				if ids[key] {
					return errors.New("duplicate saved resource ID")
				}
				ids[key] = true
			}
			if slot.Power != provider.Running && slot.Power != provider.Stopped && slot.Power != provider.Unknown {
				return errors.New("invalid resource power state")
			}
			if slot.Operation != nil {
				if !slices.Contains([]string{"create", "await-running", "resume", "stop", "destroy", "unresolved"}, slot.Operation.Kind) || slot.Operation.ID == "" || slot.Operation.StartedAt.IsZero() || !slices.Contains([]string{"", "drained", "requested"}, slot.Operation.Phase) {
					return errors.New("invalid saved operation")
				}
				if binding.Lifecycle == provider.Disposable && (slot.Operation.Kind == "resume" || slot.Operation.Kind == "stop") {
					return errors.New("disposable resource contains a retained lifecycle operation")
				}
				if binding.Lifecycle == provider.Retained && slot.Operation.Kind == "destroy" {
					return errors.New("retained resource contains a disposable lifecycle operation")
				}
				if slices.Contains([]string{"create", "unresolved"}, slot.Operation.Kind) && (slot.ResourceID != "" || slot.Operation.Phase != "") {
					return errors.New("invalid unresolved creation state")
				}
				if slices.Contains([]string{"await-running", "resume", "stop", "destroy"}, slot.Operation.Kind) && slot.ResourceID == "" {
					return errors.New("operation omitted its resource identity")
				}
				if slices.Contains([]string{"resume", "await-running"}, slot.Operation.Kind) && slot.Operation.Phase == "drained" {
					return errors.New("activation cannot have a drained release phase")
				}
			}
		}
	}
	for key, job := range s.Jobs {
		if key != job.Key() || job.ID <= 0 || job.ProjectID <= 0 {
			return fmt.Errorf("invalid saved job %s", key)
		}
	}
	for key, hint := range s.Hints {
		if key != JobKey(hint.ProjectID, hint.ID) || hint.ID <= 0 || hint.ProjectID <= 0 {
			return errors.New("invalid saved job hint")
		}
	}
	if len(s.Audit) > 1000 {
		return errors.New("audit history exceeds retention bound")
	}
	for _, record := range s.Audit {
		if record.ID == "" || record.At.IsZero() || record.Pool == "" || record.Slot < 0 || !record.Binding.Valid() || !slices.Contains([]string{"release-missing", "retry-pending"}, record.Action) {
			return errors.New("invalid recovery audit record")
		}
	}
	activeProbes := map[int64]bool{}
	for key, probe := range s.PendingProbes {
		if key != PendingProbeKey(probe.RunnerID, probe.ProjectID, probe.JobID) || probe.RunnerID <= 0 || probe.ProjectID <= 0 || probe.JobID <= 0 || probe.Pool == "" || probe.Slot < 0 || probe.ReservedAt.IsZero() || probe.Remaining < 0 || probe.HostAttempts < 0 || !slices.Contains([]string{"", "opportunity_budget", "host_limit", "pool_removed"}, probe.ExhaustionReason) {
			return errors.New("invalid pending probe record")
		}
		if !probe.Exhausted {
			if activeProbes[probe.RunnerID] {
				return errors.New("runner has multiple active pending probes")
			}
			activeProbes[probe.RunnerID] = true
		}
	}
	return nil
}

func (p Pool) HasHandles() bool {
	for _, slot := range p.Slots {
		if slot.ResourceID != "" || slot.Operation != nil {
			return true
		}
	}
	return false
}

func (s State) HasHandles(name string) bool {
	for _, pool := range s.Pools {
		if pool.Provider == name && pool.HasHandles() {
			return true
		}
	}
	return false
}
