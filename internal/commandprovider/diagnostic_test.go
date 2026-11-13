//go:build linux || darwin

package commandprovider

import (
	"strings"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func TestWireDiagnosticsAndLegacyErrors(t *testing.T) {
	for _, mode := range []string{"rejection", "diagnostic-operation", "preflight-without-description"} {
		t.Run(mode, func(t *testing.T) {
			env := []string{"GRC_HELPER_NO_SIDE_EFFECT=true"}
			if mode != "rejection" {
				env = append(env, `GRC_HELPER_DIAGNOSTIC_JSON={"operation":"hosts.create","http_status":403,"code":"AccessDenied"}`)
			}
			client := newFixtureClient(t, mode, provider.Retained, env...)
			describeFixture(t, client)
			_, err := client.Create(t.Context(), fixtureCreate())
			remote := requireKind(t, err, provider.Invalid)
			if !remote.NoSideEffect || remote.RetryAfter != 1250*time.Millisecond {
				t.Fatal("diagnostic changed failure guarantees")
			}
			if mode == "rejection" {
				if remote.Diagnostic != nil || strings.Contains(err.Error(), "call=") {
					t.Fatal("legacy adapter gained a diagnostic")
				}
			} else if remote.Diagnostic == nil || !strings.Contains(err.Error(), "(invalid); call=hosts.create http_status=403 code=AccessDenied") {
				t.Fatalf("remote diagnostic missing from error: %v", err)
			}
		})
	}
}

func TestMalformedWireDiagnosticKeepsCreateUncertain(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"operation":"hosts.create","http_status":600}`,
		`{"operation":"hosts.create","http_status":"403"}`,
		`{"operation":"hosts.create","code":"fixture-secret\nbody"}`,
		`{"operation":"https://example.test/hosts"}`,
		`{"operation":"hosts.create","response_body":"fixture-secret"}`,
		`{"operation":"` + strings.Repeat("a", provider.MaxDiagnosticTokenBytes+1) + `"}`,
	} {
		client := newFixtureClient(t, "diagnostic-malformed", provider.Retained, "GRC_HELPER_NO_SIDE_EFFECT=true", "GRC_HELPER_DIAGNOSTIC_JSON="+raw)
		describeFixture(t, client)
		_, err := client.Create(t.Context(), fixtureCreate())
		remote := requireKind(t, err, provider.Uncertain)
		if remote.NoSideEffect || remote.Diagnostic != nil {
			t.Fatal("malformed response certified absence or exposed diagnostic data")
		}
	}
}

func TestWireDiagnosticKnownSecretsAreOmitted(t *testing.T) {
	for _, secret := range []string{"fixture-secret", "private-credential"} {
		client := newFixtureClient(t, "diagnostic-secret", provider.Retained, "API_TOKEN=private-credential", "GRC_HELPER_NO_SIDE_EFFECT=true", `GRC_HELPER_DIAGNOSTIC_JSON={"operation":"hosts.create","code":"prefix-`+secret+`"}`)
		describeFixture(t, client)
		_, err := client.Create(t.Context(), fixtureCreate())
		remote := requireKind(t, err, provider.Invalid)
		if remote.Diagnostic != nil || !remote.NoSideEffect || strings.Contains(err.Error(), secret) {
			t.Fatal("known secret was exposed or changed failure guarantees")
		}
	}
}

func TestWireDiagnosticShortEnvironmentValuesPreserveMetadata(t *testing.T) {
	client := newFixtureClient(t, "diagnostic-operation", provider.Retained,
		"LIFECYCLE=retained", "FLAG=1", "GRC_HELPER_NO_SIDE_EFFECT=true",
		`GRC_HELPER_DIAGNOSTIC_JSON={"operation":"retained.create.v1","http_status":403,"code":"E1"}`)
	describeFixture(t, client)
	_, err := client.Create(t.Context(), fixtureCreate())
	remote := requireKind(t, err, provider.Invalid)
	if remote.Diagnostic == nil || !strings.Contains(err.Error(), "call=retained.create.v1 http_status=403 code=E1") || !remote.NoSideEffect || remote.RetryAfter != 1250*time.Millisecond {
		t.Fatalf("short environment values hid metadata or changed failure guarantees: %v", err)
	}
}
