// Package provider defines the runner-hosting contract used by the controller.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type PowerState string

const (
	Running PowerState = "running"
	Stopped PowerState = "stopped"
	Unknown PowerState = "unknown"
)

type Lifecycle string

const (
	Retained   Lifecycle = "retained"
	Disposable Lifecycle = "disposable"
)

type Description struct {
	Identity               string    `json:"identity"`
	Scope                  string    `json:"scope"`
	Lifecycle              Lifecycle `json:"lifecycle"`
	Revision               string    `json:"revision"`
	AbsenceConfirmsDestroy bool      `json:"absence_confirms_destroy"`
}

func (d Description) AccountKey() string { return d.Scope + "\x00" + d.Identity }

type Resource struct {
	ID        string     `json:"id"`
	State     PowerState `json:"state"`
	CreatedAt time.Time  `json:"created_at,omitempty"`
}

// RunnerSpec is transient. Tokens must never appear in inventory, errors, or logs.
type RunnerSpec struct {
	Name      string          `json:"name"`
	GitLabURL string          `json:"gitlab_url"`
	Token     string          `json:"token"`
	Jobs      int             `json:"jobs"`
	Options   json.RawMessage `json:"options"`
	Revision  string          `json:"revision"`
}

type CreateRequest struct {
	OperationID string
	Options     json.RawMessage
	Runner      RunnerSpec
}

// Provider implementations must list all account resources for capacity accounting.
// Resume, Persist, Configure, Drain, Stop, and Destroy must be safe to repeat after a lost response.
// Create must return Uncertain when a request may have created an unknown resource.
// A missing resource must never be reported as stopped.
type Provider interface {
	ValidateOptions(json.RawMessage) error
	Describe(context.Context) (Description, error)
	List(context.Context) ([]Resource, error)
	Create(context.Context, CreateRequest) (Resource, error)
	Resume(context.Context, string) error
	Persist(context.Context, string) error
	Healthy(context.Context, string, json.RawMessage, string) (bool, error)
	Configure(context.Context, string, json.RawMessage, RunnerSpec) (bool, error)
	// Drain stops accepting jobs and returns true only after in-flight jobs finish.
	Drain(context.Context, string, json.RawMessage, string) (bool, error)
	Stop(context.Context, string) error
	Destroy(context.Context, string) error
}

type ErrorKind string

const (
	Transient ErrorKind = "transient"
	Uncertain ErrorKind = "uncertain"
	NotFound  ErrorKind = "not_found"
	Conflict  ErrorKind = "conflict"
	Invalid   ErrorKind = "invalid"
)

// Error contains only operator-safe text, never a raw response or command output.
type Error struct {
	Operation    string
	Kind         ErrorKind
	RetryAfter   time.Duration
	Detail       string
	NoSideEffect bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider %s: %s (%s)", e.Operation, e.Detail, e.Kind)
}
