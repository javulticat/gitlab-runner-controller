package provider_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func TestDiagnosticBoundsAndSecretFiltering(t *testing.T) {
	for _, test := range []struct {
		name       string
		diagnostic *provider.Diagnostic
		env        []string
		token      string
		valid      bool
	}{
		{name: "omitted"},
		{name: "call only", diagnostic: &provider.Diagnostic{Operation: "hosts.create"}, valid: true},
		{name: "http", diagnostic: &provider.Diagnostic{Operation: "hosts.create", HTTPStatus: 403, Code: "AccessDenied"}, valid: true},
		{name: "lower status", diagnostic: &provider.Diagnostic{Operation: "hosts.create", HTTPStatus: 100}, valid: true},
		{name: "upper bounds", diagnostic: &provider.Diagnostic{Operation: strings.Repeat("a", provider.MaxDiagnosticTokenBytes), Code: strings.Repeat("Z", provider.MaxDiagnosticTokenBytes), HTTPStatus: 599}, valid: true},
		{name: "empty call", diagnostic: &provider.Diagnostic{HTTPStatus: 403}},
		{name: "long call", diagnostic: &provider.Diagnostic{Operation: strings.Repeat("a", provider.MaxDiagnosticTokenBytes+1)}},
		{name: "long code", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: strings.Repeat("a", provider.MaxDiagnosticTokenBytes+1)}},
		{name: "low status", diagnostic: &provider.Diagnostic{Operation: "hosts.create", HTTPStatus: 99}},
		{name: "high status", diagnostic: &provider.Diagnostic{Operation: "hosts.create", HTTPStatus: 600}},
		{name: "negative status", diagnostic: &provider.Diagnostic{Operation: "hosts.create", HTTPStatus: -1}},
		{name: "url", diagnostic: &provider.Diagnostic{Operation: "https://example.test/hosts"}},
		{name: "body", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: `{"error":"denied"}`}},
		{name: "message", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: "Access denied"}},
		{name: "newline", diagnostic: &provider.Diagnostic{Operation: "hosts\ncreate"}},
		{name: "unicode separator", diagnostic: &provider.Diagnostic{Operation: "hosts\u2028create"}},
		{name: "terminal control", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: "\x1b[31mDenied"}},
		{name: "invalid utf8", diagnostic: &provider.Diagnostic{Operation: "hosts.\xffcreate"}},
		{name: "leading punctuation", diagnostic: &provider.Diagnostic{Operation: ".create"}},
		{name: "runner token", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: "prefix-fixture-secret"}, token: "fixture-secret"},
		{name: "short runner token", diagnostic: &provider.Diagnostic{Operation: "hosts.create.v1", Code: "E1"}, token: "1"},
		{name: "environment secret", diagnostic: &provider.Diagnostic{Operation: "private-credential.create"}, env: []string{"API_TOKEN=private-credential"}},
		{name: "environment flag", diagnostic: &provider.Diagnostic{Operation: "hosts.create.v1", Code: "E1"}, env: []string{"FLAG=1"}, valid: true},
		{name: "environment lifecycle", diagnostic: &provider.Diagnostic{Operation: "retained.create"}, env: []string{"LIFECYCLE=retained"}, valid: true},
		{name: "eight-byte environment value", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: "Error-12345678"}, env: []string{"SETTING=12345678"}, valid: true},
		{name: "nine-byte environment value", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: "Error-123456789"}, env: []string{"API_TOKEN=123456789"}},
		{name: "environment defaults", diagnostic: &provider.Diagnostic{Operation: "hosts.create", Code: "CONFLICT"}, env: []string{"LANG=C", "PATH=/usr/bin:/bin", "EMPTY="}, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := provider.SanitizeDiagnostic(test.diagnostic, test.env, test.token)
			if (got != nil) != test.valid {
				t.Fatalf("diagnostic accepted = %t, want %t", got != nil, test.valid)
			}
			if got != nil && (*got != *test.diagnostic || got == test.diagnostic) {
				t.Fatal("sanitization did not preserve an independent diagnostic value")
			}
		})
	}
}

func TestServeExplicitDiagnosticsPreserveFailureMetadata(t *testing.T) {
	t.Setenv("GRC_DIAGNOSTIC_LIFECYCLE", "retained")
	t.Setenv("GRC_DIAGNOSTIC_FLAG", "1")
	for _, stage := range []string{"create", "describe", "options"} {
		t.Run(stage, func(t *testing.T) {
			fixture, request := newProtocolFixture(), protocolRequest()
			diagnostic := &provider.Diagnostic{Operation: "retained.create.v1", HTTPStatus: 403, Code: "E1"}
			failure := fmt.Errorf("fixture-secret: %w", &provider.Error{Kind: provider.Invalid, Detail: "fixture-secret", Diagnostic: diagnostic, NoSideEffect: true, RetryAfter: time.Second})
			switch stage {
			case "create":
				fixture.createError = failure
			case "describe":
				fixture.describeError = failure
			case "options":
				fixture.optionsError = failure
				request.Options = []byte(`{}`)
			}
			response := serveRequest(t, fixture, request)
			remote := response.Error
			if remote == nil || remote.Diagnostic == nil || *remote.Diagnostic != *diagnostic || remote.Kind != provider.Invalid || !remote.NoSideEffect || remote.RetryAfterMS != 1000 {
				t.Fatalf("diagnostic or failure metadata lost: %+v", remote)
			}
		})
	}
}

func TestServeOmitsUnsafeDiagnosticsWithoutChangingFailure(t *testing.T) {
	t.Setenv("GRC_DIAGNOSTIC_SECRET", "private-credential")
	for _, diagnostic := range []*provider.Diagnostic{
		nil,
		{Operation: "hosts.create", Code: "fixture-secret"},
		{Operation: "hosts.create", Code: "private-credential"},
		{Operation: "hosts.create", Code: "response body"},
		{Operation: strings.Repeat("a", provider.MaxDiagnosticTokenBytes+1)},
	} {
		fixture := newProtocolFixture()
		fixture.createError = &provider.Error{Kind: provider.Invalid, Detail: "fixture-secret", Diagnostic: diagnostic, NoSideEffect: true, RetryAfter: time.Second}
		remote := serveRequest(t, fixture, protocolRequest()).Error
		if remote == nil || remote.Diagnostic != nil || remote.Kind != provider.Invalid || !remote.NoSideEffect || remote.RetryAfterMS != 1000 || remote.Stage != "operation" {
			t.Fatalf("unsafe diagnostic changed failure semantics: %+v", remote)
		}
	}
}
