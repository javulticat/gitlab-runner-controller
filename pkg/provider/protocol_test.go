package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type protocolFixture struct {
	description           provider.Description
	createCalls           int
	resumeCalls           int
	persistCalls          int
	stopCalls             int
	destroyCalls          int
	createError           error
	createResourceOnError bool
	describeError         error
	optionsError          error
	readinessError        error
	list                  []provider.Resource
}

func newProtocolFixture() *protocolFixture {
	return &protocolFixture{description: provider.Description{Identity: "fixture", Scope: "account-1", Lifecycle: provider.Retained, Revision: "fixture-v1"}, list: []provider.Resource{}}
}

func (p *protocolFixture) ValidateOptions(json.RawMessage) error { return p.optionsError }
func (p *protocolFixture) Describe(context.Context) (provider.Description, error) {
	if p.describeError != nil {
		return provider.Description{}, p.describeError
	}
	return p.description, nil
}
func (p *protocolFixture) List(context.Context) ([]provider.Resource, error) { return p.list, nil }
func (p *protocolFixture) Create(context.Context, provider.CreateRequest) (provider.Resource, error) {
	p.createCalls++
	if p.createError != nil && !p.createResourceOnError {
		return provider.Resource{}, p.createError
	}
	return provider.Resource{ID: "created-1", State: provider.Running}, p.createError
}
func (p *protocolFixture) Resume(context.Context, string) error  { p.resumeCalls++; return nil }
func (p *protocolFixture) Persist(context.Context, string) error { p.persistCalls++; return nil }
func (p *protocolFixture) Stop(context.Context, string) error    { p.stopCalls++; return nil }
func (p *protocolFixture) Destroy(context.Context, string) error { p.destroyCalls++; return nil }
func (p *protocolFixture) Healthy(context.Context, string, json.RawMessage, string) (bool, error) {
	return false, p.readinessError
}
func (p *protocolFixture) Configure(context.Context, string, json.RawMessage, provider.RunnerSpec) (bool, error) {
	return true, p.readinessError
}
func (p *protocolFixture) Drain(context.Context, string, json.RawMessage, string) (bool, error) {
	return true, p.readinessError
}

func protocolRequest() provider.Request {
	return provider.Request{Version: 1, ID: "request-1", Operation: "create", Deadline: time.Now().Add(time.Second), Expected: newProtocolFixture().description, OperationID: "intent-1", Runner: &provider.RunnerSpec{Token: "fixture-secret"}}
}

func serveRequest(t *testing.T, fixture *protocolFixture, request provider.Request) provider.Response {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := provider.Serve(t.Context(), bytes.NewReader(data), &output, func() (provider.Provider, error) { return fixture, nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "fixture-secret") {
		t.Fatal("SDK exposed an implementation error or token")
	}
	var response provider.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Version != 1 || response.ID != request.ID || response.Operation != request.Operation {
		t.Fatalf("response did not echo request envelope: %+v", response)
	}
	return response
}

func TestServeChecksExpectedBindingBeforeCreate(t *testing.T) {
	for _, field := range []string{"identity", "scope", "lifecycle", "absence", "invalid-description"} {
		t.Run(field, func(t *testing.T) {
			fixture, request := newProtocolFixture(), protocolRequest()
			switch field {
			case "identity":
				fixture.description.Identity = "other-provider"
			case "scope":
				fixture.description.Scope = "other-account"
			case "lifecycle":
				fixture.description.Lifecycle, fixture.description.AbsenceConfirmsDestroy = provider.Disposable, true
			case "absence":
				fixture.description.AbsenceConfirmsDestroy = true
			case "invalid-description":
				fixture.description.Revision = ""
			}
			response := serveRequest(t, fixture, request)
			if fixture.createCalls != 0 || response.Error == nil || response.Error.Kind != provider.Invalid || !response.Error.NoSideEffect || response.Error.Stage != "preflight" {
				t.Fatalf("binding failure was not rejected before creation: calls=%d response=%+v", fixture.createCalls, response)
			}
		})
	}
}

func TestServeEnforcesLifecycleBeforeMutation(t *testing.T) {
	for _, operation := range []string{"resume", "persist", "stop", "destroy"} {
		t.Run(operation, func(t *testing.T) {
			fixture, request := newProtocolFixture(), protocolRequest()
			request.Operation, request.ResourceID = operation, "resource-1"
			if operation != "destroy" {
				fixture.description.Lifecycle, fixture.description.AbsenceConfirmsDestroy = provider.Disposable, true
			}
			request.Expected = fixture.description
			response := serveRequest(t, fixture, request)
			if fixture.resumeCalls+fixture.persistCalls+fixture.stopCalls+fixture.destroyCalls != 0 || response.Error == nil || response.Error.Kind != provider.Invalid || !response.Error.NoSideEffect {
				t.Fatalf("lifecycle failure reached provider: %+v", response)
			}
		})
	}
}

func TestServeCreateErrorsHaveOnlySafeMetadata(t *testing.T) {
	for _, test := range []struct {
		name         string
		err          error
		kind         provider.ErrorKind
		noSideEffect bool
	}{
		{name: "untyped", err: errors.New("fixture-secret"), kind: provider.Uncertain},
		{name: "typed", err: &provider.Error{Kind: provider.Invalid, Detail: "fixture-secret"}, kind: provider.Invalid},
		{name: "rejected", err: &provider.Error{Kind: provider.Invalid, Detail: "fixture-secret", NoSideEffect: true}, kind: provider.Invalid, noSideEffect: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProtocolFixture()
			fixture.createError = test.err
			response := serveRequest(t, fixture, protocolRequest())
			if response.Error == nil || response.Error.Kind != test.kind || response.Error.NoSideEffect != test.noSideEffect || response.Error.Stage != "operation" || response.Resource != nil || fixture.createCalls != 1 {
				t.Fatalf("unexpected error response: %+v", response)
			}
		})
	}
}

func TestServePreflightErrorsCertifyNoDispatch(t *testing.T) {
	for _, phase := range []string{"factory", "describe", "options"} {
		t.Run(phase, func(t *testing.T) {
			fixture, request := newProtocolFixture(), protocolRequest()
			preflightErr := &provider.Error{Kind: provider.Transient, RetryAfter: 1250 * time.Millisecond, Detail: "fixture-secret"}
			if phase == "describe" {
				fixture.describeError = preflightErr
			}
			if phase == "options" {
				fixture.optionsError = preflightErr
				request.Options = json.RawMessage(`{}`)
			}
			data, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err = provider.Serve(t.Context(), bytes.NewReader(data), &output, func() (provider.Provider, error) {
				if phase == "factory" {
					return nil, preflightErr
				}
				return fixture, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "fixture-secret") {
				t.Fatal("preflight error exposed implementation detail")
			}
			var response provider.Response
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if fixture.createCalls != 0 || response.Error == nil || response.Error.Stage != "preflight" || !response.Error.NoSideEffect || response.Error.Kind != provider.Transient || response.Error.RetryAfterMS != 1250 {
				t.Fatalf("preflight error lost retry metadata or dispatched a mutation: calls=%d response=%+v", fixture.createCalls, response)
			}
			if phase != "options" && response.Description != (provider.Description{}) {
				t.Fatalf("failed factory or profile lookup unexpectedly supplied description: %+v", response.Description)
			}
		})
	}
}

func TestServeOperationErrorsClearSuccessPayloads(t *testing.T) {
	for _, operation := range []string{"healthy", "configure", "drain"} {
		t.Run(operation, func(t *testing.T) {
			fixture, request := newProtocolFixture(), protocolRequest()
			fixture.readinessError = &provider.Error{Kind: provider.Transient, RetryAfter: 1250 * time.Millisecond, Detail: "fixture-secret"}
			request.Operation, request.ResourceID, request.Owner = operation, "resource-1", "owner"
			response := serveRequest(t, fixture, request)
			if response.Error == nil || response.Error.Stage != "operation" || response.Error.Kind != provider.Transient || response.Error.RetryAfterMS != 1250 {
				t.Fatalf("operation error lost metadata: %+v", response)
			}
			requireNoSuccessPayload(t, response)
		})
	}
}

func TestServeContradictoryCreateOutcomeIsUncertain(t *testing.T) {
	fixture := newProtocolFixture()
	fixture.createError = &provider.Error{Kind: provider.Invalid, NoSideEffect: true, Detail: "fixture-secret"}
	fixture.createResourceOnError = true
	response := serveRequest(t, fixture, protocolRequest())
	if response.Error == nil || response.Error.Stage != "operation" || response.Error.Kind != provider.Uncertain || response.Error.NoSideEffect {
		t.Fatalf("contradictory creation outcome claimed certainty: %+v", response)
	}
	requireNoSuccessPayload(t, response)
}

func requireNoSuccessPayload(t *testing.T, response provider.Response) {
	t.Helper()
	if response.Ready != nil || response.Resource != nil || response.Resources != nil || response.Complete != nil || response.OK != nil {
		t.Fatalf("error response contains success payloads: %+v", response)
	}
}

func TestServeEmptyInventoryIsComplete(t *testing.T) {
	fixture, request := newProtocolFixture(), protocolRequest()
	request.Operation = "list"
	response := serveRequest(t, fixture, request)
	if response.Error != nil || response.Resources == nil || len(response.Resources) != 0 || response.Complete == nil || !*response.Complete {
		t.Fatalf("empty inventory was incomplete: %+v", response)
	}
	fixture.list = nil
	response = serveRequest(t, fixture, request)
	if response.Error == nil || response.Complete != nil {
		t.Fatalf("nil inventory claimed completeness: %+v", response)
	}
}

func TestServeRejectsMalformedEnvelopeBeforeFactory(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `{}`, `{"version":2,"id":"request-1","operation":"create","deadline":"2030-01-01T00:00:00Z"}`, `{"version":1,"id":"request-1","operation":"describe","deadline":"2030-01-01T00:00:00Z","extra":"fixture-secret"}`, `{} {}`, strings.Repeat(" ", provider.MaxMessageBytes+1)} {
		factoryCalls := 0
		err := provider.Serve(t.Context(), strings.NewReader(input), io.Discard, func() (provider.Provider, error) { factoryCalls++; return newProtocolFixture(), nil })
		if err == nil || factoryCalls != 0 {
			t.Fatalf("invalid envelope reached factory: error=%v calls=%d input=%.60q", err, factoryCalls, input)
		}
	}
}

func TestDecodeMessageRejectsUnknownAndTrailingData(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `{"unexpected":"fixture-secret"}`, `{}`, `{} {}`, strings.Repeat(" ", provider.MaxMessageBytes+1)} {
		var response provider.Response
		err := provider.DecodeMessage([]byte(input), &response)
		if input == "{}" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatalf("invalid protocol value accepted: %.60q", input)
		}
	}
}

func TestDescriptionRequiresDisposableAbsenceGuarantee(t *testing.T) {
	description := newProtocolFixture().description
	description.Lifecycle = provider.Disposable
	if description.Valid() {
		t.Fatal("disposable provider accepted without an absence guarantee")
	}
	description.AbsenceConfirmsDestroy = true
	if !description.Valid() {
		t.Fatal("valid disposable description rejected")
	}
}
