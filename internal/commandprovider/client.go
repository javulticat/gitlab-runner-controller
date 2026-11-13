// Package commandprovider transports provider operations through an executable.
package commandprovider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

const (
	maxStderrBytes = 32 << 10
	pipeWaitDelay  = 250 * time.Millisecond
)

var errOutputLimit = errors.New("command output exceeds protocol limits")

// Client starts a separate process for each request and pins its account scope.
type Client struct {
	command   []string
	env       []string
	timeout   time.Duration
	mu        sync.Mutex
	expected  provider.Description
	described bool
}

var _ provider.Provider = (*Client)(nil)

// New validates transport settings without executing the provider.
func New(command, env []string, expected provider.Description, timeout time.Duration) (*Client, error) {
	if len(command) == 0 || len(command) > 128 || !filepath.IsAbs(command[0]) {
		return nil, errors.New("provider command requires an absolute executable path")
	}
	for _, arg := range command {
		if len(arg) > 64<<10 || strings.ContainsRune(arg, '\x00') {
			return nil, errors.New("provider command contains an invalid argument")
		}
	}
	if expected.Identity == "" || len(expected.Identity) > 256 || strings.ContainsAny(expected.Identity, "\r\n\x00") || (expected.Lifecycle != provider.Retained && expected.Lifecycle != provider.Disposable) || timeout <= 0 {
		return nil, errors.New("provider command requires a valid identity, lifecycle, and timeout")
	}
	if err := supportedPlatform(); err != nil {
		return nil, err
	}
	resolvedEnv, err := commandEnvironment(env)
	if err != nil {
		return nil, err
	}
	return &Client{command: slices.Clone(command), env: resolvedEnv, expected: expected, timeout: timeout}, nil
}

func commandEnvironment(env []string) ([]string, error) {
	result := make([]string, 0, len(env)+2)
	keys := make(map[string]bool, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, '\x00') || keys[key] {
			return nil, errors.New("provider command environment has invalid or duplicate entries")
		}
		for i, r := range key {
			if r != '_' && (r < 'A' || r > 'Z') && (i == 0 || r < '0' || r > '9') {
				return nil, errors.New("provider command environment has an invalid variable name")
			}
		}
		keys[key] = true
		result = append(result, entry)
	}
	if !keys["PATH"] {
		result = append(result, "PATH=/usr/bin:/bin")
	}
	if !keys["LANG"] {
		result = append(result, "LANG=C")
	}
	return result, nil
}

// ValidateOptions checks only the generic JSON boundary; the executable owns its schema.
func (*Client) ValidateOptions(options json.RawMessage) error {
	if len(options) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if len(options) > provider.MaxMessageBytes || json.Unmarshal(options, &object) != nil || object == nil {
		return localError("validate_options", "provider options must be a JSON object")
	}
	return nil
}

func (c *Client) Describe(ctx context.Context) (provider.Description, error) {
	response, err := c.call(ctx, provider.Request{Operation: "describe"})
	if err != nil {
		return provider.Description{}, err
	}
	return response.Description, nil
}

func (c *Client) List(ctx context.Context) ([]provider.Resource, error) {
	response, err := c.call(ctx, provider.Request{Operation: "list"})
	if err != nil {
		return nil, err
	}
	if response.Complete == nil || !*response.Complete || response.Resources == nil {
		return nil, transportError("list", "provider inventory is incomplete")
	}
	seen := make(map[string]bool, len(response.Resources))
	for _, resource := range response.Resources {
		if !validResource(resource) || seen[resource.ID] {
			return nil, transportError("list", "provider inventory contains invalid or duplicate resources")
		}
		seen[resource.ID] = true
	}
	return response.Resources, nil
}

func (c *Client) Create(ctx context.Context, request provider.CreateRequest) (provider.Resource, error) {
	if request.OperationID == "" {
		return provider.Resource{}, localError("create", "creation requires an operation ID")
	}
	response, err := c.call(ctx, provider.Request{Operation: "create", OperationID: request.OperationID, Options: request.Options, Runner: &request.Runner})
	if err != nil {
		return provider.Resource{}, err
	}
	if response.Resource == nil || !validResource(*response.Resource) || response.OK == nil || !*response.OK {
		return provider.Resource{}, transportError("create", "provider creation returned an invalid resource")
	}
	return *response.Resource, nil
}

func (c *Client) Resume(ctx context.Context, id string) error {
	return c.confirm(ctx, "resume", id)
}

func (c *Client) Persist(ctx context.Context, id string) error {
	return c.confirm(ctx, "persist", id)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.confirm(ctx, "stop", id)
}

func (c *Client) Destroy(ctx context.Context, id string) error {
	return c.confirm(ctx, "destroy", id)
}

func (c *Client) confirm(ctx context.Context, operation, id string) error {
	response, err := c.call(ctx, provider.Request{Operation: operation, ResourceID: id})
	if err != nil {
		return err
	}
	if response.OK == nil || !*response.OK {
		return transportError(operation, "provider did not confirm the operation")
	}
	return nil
}

func (c *Client) Healthy(ctx context.Context, id string, options json.RawMessage, owner string) (bool, error) {
	return c.readiness(ctx, provider.Request{Operation: "healthy", ResourceID: id, Options: options, Owner: owner})
}

func (c *Client) Configure(ctx context.Context, id string, options json.RawMessage, runner provider.RunnerSpec) (bool, error) {
	return c.readiness(ctx, provider.Request{Operation: "configure", ResourceID: id, Options: options, Runner: &runner})
}

func (c *Client) Drain(ctx context.Context, id string, options json.RawMessage, owner string) (bool, error) {
	return c.readiness(ctx, provider.Request{Operation: "drain", ResourceID: id, Options: options, Owner: owner})
}

func (c *Client) readiness(ctx context.Context, request provider.Request) (bool, error) {
	response, err := c.call(ctx, request)
	if err != nil {
		return false, err
	}
	if response.Ready == nil {
		return false, transportError(request.Operation, "provider omitted readiness")
	}
	return *response.Ready, nil
}

func (c *Client) call(ctx context.Context, request provider.Request) (provider.Response, error) {
	c.mu.Lock()
	expected, described := c.expected, c.described
	c.mu.Unlock()
	if request.Operation != "describe" && !described {
		return provider.Response{}, localError(request.Operation, "provider description must be verified first")
	}
	if (expected.Lifecycle == provider.Disposable && (request.Operation == "resume" || request.Operation == "persist" || request.Operation == "stop")) || (expected.Lifecycle == provider.Retained && request.Operation == "destroy") {
		return provider.Response{}, localError(request.Operation, "operation conflicts with configured lifecycle")
	}
	if request.Operation != "describe" && request.Operation != "list" && request.Operation != "create" && request.ResourceID == "" {
		return provider.Response{}, localError(request.Operation, "operation requires a resource ID")
	}
	if (request.Operation == "healthy" || request.Operation == "drain") && request.Owner == "" {
		return provider.Response{}, localError(request.Operation, "operation requires an owner identity")
	}
	if err := c.ValidateOptions(request.Options); err != nil {
		return provider.Response{}, localError(request.Operation, "provider options must be a JSON object")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request.Version = provider.ProtocolVersion
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return provider.Response{}, localError(request.Operation, "could not generate a request ID")
	}
	request.ID, request.Expected = hex.EncodeToString(id[:]), expected
	request.Deadline, _ = ctx.Deadline()
	data, err := json.Marshal(request)
	if err != nil || len(data)+1 > provider.MaxMessageBytes {
		return provider.Response{}, localError(request.Operation, "provider request exceeds protocol limits or contains invalid JSON")
	}
	command := exec.CommandContext(ctx, c.command[0], c.command[1:]...)
	command.Env = c.env
	command.Stdin = bytes.NewReader(append(data, '\n'))
	command.WaitDelay = pipeWaitDelay
	prepareCommand(command)
	command.Cancel = func() error { return terminateCommand(command) }
	stdout := &boundedBuffer{limit: provider.MaxMessageBytes, cancel: cancel}
	stderr := &boundedBuffer{limit: maxStderrBytes, cancel: cancel}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		if cleanupErr := terminateCommand(command); cleanupErr != nil {
			return provider.Response{}, transportError(request.Operation, "provider process cleanup failed")
		}
		return provider.Response{}, transportError(request.Operation, "provider process did not complete successfully")
	}
	if stdout.exceeded || stderr.exceeded || ctx.Err() != nil {
		return provider.Response{}, transportError(request.Operation, "provider process exceeded request limits")
	}
	var response provider.Response
	if provider.DecodeMessage(stdout.Bytes(), &response) != nil || response.Version != request.Version || response.ID != request.ID || response.Operation != request.Operation {
		return provider.Response{}, transportError(request.Operation, "provider returned an invalid protocol response")
	}
	if remote := response.Error; remote != nil {
		if !validWireError(response) {
			return provider.Response{}, transportError(request.Operation, "provider returned an invalid error response")
		}
		if remote.Stage == "preflight" {
			return provider.Response{}, remoteError(request.Operation, *remote)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !response.Description.Valid() || response.Description.Identity != c.expected.Identity || response.Description.Lifecycle != c.expected.Lifecycle || (c.expected.Scope != "" && response.Description.Scope != c.expected.Scope) || (c.described && response.Description.AbsenceConfirmsDestroy != c.expected.AbsenceConfirmsDestroy) {
		return provider.Response{}, transportError(request.Operation, "provider identity, scope, or lifecycle changed")
	}
	if remote := response.Error; remote != nil {
		return provider.Response{}, remoteError(request.Operation, *remote)
	}
	if request.Operation == "describe" {
		c.expected.Scope = response.Description.Scope
		c.expected.AbsenceConfirmsDestroy = response.Description.AbsenceConfirmsDestroy
		c.described = true
	}
	return response, nil
}

func validResource(resource provider.Resource) bool {
	return resource.ID != "" && (resource.State == provider.Running || resource.State == provider.Stopped || resource.State == provider.Unknown)
}

func validErrorKind(kind provider.ErrorKind) bool {
	switch kind {
	case provider.Transient, provider.Uncertain, provider.NotFound, provider.Conflict, provider.Invalid:
		return true
	default:
		return false
	}
}

func validWireError(response provider.Response) bool {
	remote := response.Error
	if !validErrorKind(remote.Kind) || remote.RetryAfterMS < 0 || remote.RetryAfterMS > (24*time.Hour).Milliseconds() || response.Resource != nil || response.Resources != nil || response.Complete != nil || response.OK != nil || response.Ready != nil {
		return false
	}
	switch remote.Stage {
	case "preflight":
		return remote.NoSideEffect
	case "operation":
		return true
	default:
		return false
	}
}

func remoteError(operation string, remote provider.WireError) *provider.Error {
	kind := remote.Kind
	if operation == "create" && !remote.NoSideEffect {
		kind = provider.Uncertain
	}
	return &provider.Error{Operation: operation, Kind: kind, RetryAfter: time.Duration(remote.RetryAfterMS) * time.Millisecond, NoSideEffect: remote.NoSideEffect, Detail: "provider rejected the operation"}
}

func localError(operation, detail string) *provider.Error {
	return &provider.Error{Operation: operation, Kind: provider.Invalid, NoSideEffect: true, Detail: detail}
}

func transportError(operation, detail string) *provider.Error {
	kind := provider.Transient
	if operation == "create" {
		kind = provider.Uncertain
	}
	return &provider.Error{Operation: operation, Kind: kind, Detail: detail}
}

type boundedBuffer struct {
	data     []byte
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - len(b.data)
	if len(data) <= remaining {
		b.data = append(b.data, data...)
		return len(data), nil
	}
	b.data = append(b.data, data[:remaining]...)
	b.exceeded = true
	b.cancel()
	return remaining, errOutputLimit
}

func (b *boundedBuffer) Bytes() []byte { return b.data }
