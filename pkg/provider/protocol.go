package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const ProtocolVersion = 1
const MaxMessageBytes = 4 << 20

type Request struct {
	Version     int             `json:"version"`
	ID          string          `json:"id"`
	Operation   string          `json:"operation"`
	Expected    Description     `json:"expected"`
	Deadline    time.Time       `json:"deadline"`
	ResourceID  string          `json:"resource_id,omitempty"`
	OperationID string          `json:"operation_id,omitempty"`
	Options     json.RawMessage `json:"options,omitempty"`
	Runner      *RunnerSpec     `json:"runner,omitempty"`
	Owner       string          `json:"owner,omitempty"`
}

type WireError struct {
	Kind         ErrorKind `json:"kind"`
	RetryAfterMS int64     `json:"retry_after_ms,omitempty"`
	NoSideEffect bool      `json:"no_side_effect,omitempty"`
	Stage        string    `json:"stage"`
}

type Response struct {
	Version     int         `json:"version"`
	ID          string      `json:"id"`
	Operation   string      `json:"operation"`
	Description Description `json:"description"`
	Error       *WireError  `json:"error,omitempty"`
	Resources   []Resource  `json:"resources"`
	Resource    *Resource   `json:"resource,omitempty"`
	Complete    *bool       `json:"complete,omitempty"`
	Ready       *bool       `json:"ready,omitempty"`
	OK          *bool       `json:"ok,omitempty"`
}

func DecodeMessage(data []byte, target any) error {
	if len(data) > MaxMessageBytes || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("invalid protocol message size or null value")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid protocol JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one protocol message")
	}
	return nil
}

func (d Description) Valid() bool {
	return d.Identity != "" && d.Scope != "" && d.Revision != "" && len(d.Identity) <= 256 && len(d.Scope) <= 256 && len(d.Revision) <= 256 && !strings.ContainsAny(d.Identity+d.Scope+d.Revision, "\r\n\x00") && (d.Lifecycle == Retained || (d.Lifecycle == Disposable && d.AbsenceConfirmsDestroy))
}

// Serve runs one operation. Errors carry categories only, never provider output.
func Serve(ctx context.Context, input io.Reader, output io.Writer, factory func() (Provider, error)) error {
	data, err := io.ReadAll(io.LimitReader(input, MaxMessageBytes+1))
	if err != nil {
		return errors.New("read protocol request")
	}
	var request Request
	if err := DecodeMessage(data, &request); err != nil {
		return err
	}
	if request.Version != ProtocolVersion || request.ID == "" || request.Operation == "" || request.Deadline.IsZero() {
		return errors.New("unsupported or incomplete protocol request")
	}
	ctx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	response := Response{Version: ProtocolVersion, ID: request.ID, Operation: request.Operation}
	instance, err := factory()
	dispatched := false
	if err == nil {
		response.Description, err = instance.Describe(ctx)
		if err == nil && (!response.Description.Valid() || (request.Expected.Identity != "" && request.Expected.Identity != response.Description.Identity) ||
			(request.Expected.Scope != "" && (request.Expected.Scope != response.Description.Scope || request.Expected.AbsenceConfirmsDestroy != response.Description.AbsenceConfirmsDestroy)) || (request.Expected.Lifecycle != "" && request.Expected.Lifecycle != response.Description.Lifecycle)) {
			err = &Error{Operation: request.Operation, Kind: Invalid, Detail: "provider identity, scope, or lifecycle mismatch", NoSideEffect: true}
		}
		if err == nil && len(request.Options) != 0 {
			err = instance.ValidateOptions(request.Options)
		}
		if err == nil {
			dispatched = true
			err = execute(ctx, instance, request, &response)
		}
	}
	if err != nil {
		response.Resources, response.Resource, response.Complete, response.Ready, response.OK = nil, nil, nil, nil, nil
		kind := Transient
		if request.Operation == "create" && dispatched {
			kind = Uncertain
		}
		var remote *Error
		wire := &WireError{Kind: kind}
		wire.Stage = "operation"
		if errors.As(err, &remote) {
			wire.Kind = remote.Kind
			wire.RetryAfterMS = min(24*time.Hour, max(0, remote.RetryAfter)).Milliseconds()
			wire.NoSideEffect = remote.NoSideEffect
		}
		if !dispatched {
			wire.Stage = "preflight"
			wire.NoSideEffect = true
		}
		response.Error = wire
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded)+1 > MaxMessageBytes {
		return errors.New("response exceeds protocol limits")
	}
	_, err = output.Write(append(encoded, '\n'))
	return err
}

func execute(ctx context.Context, p Provider, req Request, res *Response) error {
	if (res.Description.Lifecycle == Disposable && (req.Operation == "resume" || req.Operation == "persist" || req.Operation == "stop")) || (res.Description.Lifecycle == Retained && req.Operation == "destroy") {
		return &Error{Operation: req.Operation, Kind: Invalid, Detail: "operation conflicts with configured lifecycle", NoSideEffect: true}
	}
	ok := true
	switch req.Operation {
	case "describe":
		return nil
	case "list":
		resources, err := p.List(ctx)
		if err != nil {
			return err
		}
		if resources == nil {
			return &Error{Operation: req.Operation, Kind: Transient, Detail: "incomplete inventory", NoSideEffect: true}
		}
		res.Resources, res.Complete = resources, &ok
		return nil
	}
	if req.Operation != "create" && req.ResourceID == "" {
		return &Error{Operation: req.Operation, Kind: Invalid, Detail: "missing resource ID", NoSideEffect: true}
	}
	var err error
	switch req.Operation {
	case "create":
		if req.Runner == nil || req.OperationID == "" {
			return &Error{Operation: req.Operation, Kind: Invalid, Detail: "missing creation intent", NoSideEffect: true}
		}
		var resource Resource
		resource, err = p.Create(ctx, CreateRequest{OperationID: req.OperationID, Options: req.Options, Runner: *req.Runner})
		if err != nil && resource.ID != "" {
			return &Error{Operation: req.Operation, Kind: Uncertain, Detail: "creation returned both identity and failure"}
		}
		if err == nil {
			res.Resource = &resource
		}
	case "resume":
		err = p.Resume(ctx, req.ResourceID)
	case "persist":
		err = p.Persist(ctx, req.ResourceID)
	case "stop":
		err = p.Stop(ctx, req.ResourceID)
	case "destroy":
		err = p.Destroy(ctx, req.ResourceID)
	case "configure":
		if req.Runner == nil {
			return &Error{Operation: req.Operation, Kind: Invalid, Detail: "missing runner specification"}
		}
		ready, callErr := p.Configure(ctx, req.ResourceID, req.Options, *req.Runner)
		res.Ready = &ready
		return callErr
	case "healthy", "drain":
		if req.Owner == "" {
			return &Error{Operation: req.Operation, Kind: Invalid, Detail: "missing owner identity"}
		}
		var ready bool
		if req.Operation == "healthy" {
			ready, err = p.Healthy(ctx, req.ResourceID, req.Options, req.Owner)
		} else {
			ready, err = p.Drain(ctx, req.ResourceID, req.Options, req.Owner)
		}
		res.Ready = &ready
		return err
	default:
		return &Error{Operation: req.Operation, Kind: Invalid, Detail: "unsupported operation"}
	}
	if err == nil {
		res.OK = &ok
	}
	return err
}
