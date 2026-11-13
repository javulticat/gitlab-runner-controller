//go:build linux || darwin

package commandprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func newFixtureClient(t *testing.T, mode string, lifecycle provider.Lifecycle, extraEnv ...string) *Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append([]string{"GRC_COMMAND_HELPER=1", "GRC_HELPER_MODE=" + mode, "GRC_HELPER_LIFECYCLE=" + string(lifecycle), "GORACE=atexit_sleep_ms=0"}, extraEnv...)
	client, err := New([]string{executable, "-test.run=^TestCommandProcess$"}, env, provider.Description{Identity: "fixture", Lifecycle: lifecycle}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func describeFixture(t *testing.T, client *Client) provider.Description {
	t.Helper()
	description, err := client.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return description
}

func requireKind(t *testing.T, err error, kind provider.ErrorKind) *provider.Error {
	t.Helper()
	var typed *provider.Error
	if !errors.As(err, &typed) || typed.Kind != kind {
		t.Fatalf("got error %v, want kind %s", err, kind)
	}
	if strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("provider error exposed command output")
	}
	return typed
}

func fixtureCreate() provider.CreateRequest {
	return provider.CreateRequest{OperationID: "intent-1", Options: json.RawMessage(`{"size":2}`), Runner: provider.RunnerSpec{Name: "runner", GitLabURL: "https://example.test", Token: "fixture-secret", Jobs: 2, Options: json.RawMessage(`{}`), Revision: "runner-v1"}}
}

func TestSDKRoundTrip(t *testing.T) {
	for _, lifecycle := range []provider.Lifecycle{provider.Retained, provider.Disposable} {
		t.Run(string(lifecycle), func(t *testing.T) {
			client := newFixtureClient(t, "sdk", lifecycle)
			description := describeFixture(t, client)
			if description.Scope != "account-1" || description.Revision != "fixture-v1" {
				t.Fatalf("unexpected description: %+v", description)
			}
			resources, err := client.List(t.Context())
			if err != nil || len(resources) != 3 || resources[0].State != provider.Running || resources[1].State != provider.Stopped || resources[2].State != provider.Unknown {
				t.Fatalf("inventory = %+v, error = %v", resources, err)
			}
			resource, err := client.Create(t.Context(), fixtureCreate())
			if err != nil || resource.ID != "created-1" {
				t.Fatalf("create = %+v, error = %v", resource, err)
			}
			for _, operation := range []func(context.Context, string) error{client.Resume, client.Persist, client.Stop} {
				err := operation(t.Context(), resource.ID)
				if lifecycle == provider.Disposable {
					requireKind(t, err, provider.Invalid)
				} else if err != nil {
					t.Fatal(err)
				}
			}
			err = client.Destroy(t.Context(), resource.ID)
			if lifecycle == provider.Retained {
				requireKind(t, err, provider.Invalid)
			} else if err != nil {
				t.Fatal(err)
			}
			for name, operation := range map[string]func() (bool, error){
				"healthy":   func() (bool, error) { return client.Healthy(t.Context(), resource.ID, nil, "owner") },
				"configure": func() (bool, error) { return client.Configure(t.Context(), resource.ID, nil, fixtureCreate().Runner) },
				"drain":     func() (bool, error) { return client.Drain(t.Context(), resource.ID, nil, "owner") },
			} {
				ready, err := operation()
				if err != nil || !ready {
					t.Fatalf("%s readiness = %v, error = %v", name, ready, err)
				}
			}
		})
	}
}

func TestIndependentJSONResponses(t *testing.T) {
	for _, mode := range []string{"manual", "empty", "not-ready", "revision"} {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, mode, provider.Retained)
			describeFixture(t, client)
			resources, err := client.List(t.Context())
			if err != nil || resources == nil {
				t.Fatalf("list = %+v, error = %v", resources, err)
			}
			if mode == "empty" && len(resources) != 0 {
				t.Fatalf("expected empty inventory, got %+v", resources)
			}
			ready, err := client.Healthy(t.Context(), "resource-1", nil, "owner")
			if err != nil || ready == (mode == "not-ready") {
				t.Fatalf("readiness = %v, error = %v", ready, err)
			}
		})
	}
}

func TestInvalidResponsesFailClosed(t *testing.T) {
	modes := []string{"version", "id", "operation", "identity", "scope", "lifecycle", "absence", "description", "null", "array", "trailing", "unknown-field", "malformed", "oversized", "stderr-overflow", "nonzero", "nonzero-rejection", "error-kind", "retry-negative", "retry-excessive", "error-stage-missing", "error-stage-unknown", "preflight-without-guarantee", "preflight-conflicting-result", "preflight-wrong-id", "preflight-wrong-version", "preflight-wrong-operation", "preflight-invalid-kind", "preflight-invalid-retry", "preflight-nonzero", "operation-without-description"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, mode, provider.Retained)
			describeFixture(t, client)
			_, err := client.List(t.Context())
			requireKind(t, err, provider.Transient)
			_, err = client.Create(t.Context(), fixtureCreate())
			providerErr := requireKind(t, err, provider.Uncertain)
			if providerErr.NoSideEffect {
				t.Fatal("invalid exchange claimed no side effect")
			}
		})
	}
}

func TestInventoryMustBeCompleteAndValid(t *testing.T) {
	for _, mode := range []string{"incomplete", "complete-missing", "null-inventory", "id-missing", "state-invalid", "duplicates"} {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, mode, provider.Retained)
			describeFixture(t, client)
			_, err := client.List(t.Context())
			requireKind(t, err, provider.Transient)
		})
	}
}

func TestCreateNeedsValidResource(t *testing.T) {
	for _, mode := range []string{"resource-missing", "id-missing", "state-invalid", "conflicting-result", "create-confirmation-missing", "create-confirmation-false"} {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, mode, provider.Retained)
			describeFixture(t, client)
			_, err := client.Create(t.Context(), fixtureCreate())
			requireKind(t, err, provider.Uncertain)
		})
	}
}

func TestFirstDescriptionMustMatchConfiguredBinding(t *testing.T) {
	for _, mode := range []string{"version", "id", "operation", "identity", "lifecycle", "description", "null", "unknown-field"} {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, "manual", provider.Retained, "GRC_HELPER_DESCRIBE_MODE="+mode)
			_, err := client.Describe(t.Context())
			requireKind(t, err, provider.Transient)
			_, err = client.Create(t.Context(), fixtureCreate())
			if !requireKind(t, err, provider.Invalid).NoSideEffect {
				t.Fatal("failed description allowed mutation dispatch")
			}
		})
	}
}

func TestStructuredErrors(t *testing.T) {
	for _, kind := range []provider.ErrorKind{provider.Transient, provider.Uncertain, provider.NotFound, provider.Conflict, provider.Invalid} {
		for _, noSideEffect := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", kind, noSideEffect), func(t *testing.T) {
				client := newFixtureClient(t, "rejection", provider.Retained, "GRC_HELPER_KIND="+string(kind), "GRC_HELPER_NO_SIDE_EFFECT="+strconv.FormatBool(noSideEffect))
				describeFixture(t, client)
				_, err := client.List(t.Context())
				typed := requireKind(t, err, kind)
				if typed.RetryAfter != 1250*time.Millisecond || typed.NoSideEffect != noSideEffect {
					t.Fatalf("unexpected error metadata: %+v", typed)
				}
				_, err = client.Create(t.Context(), fixtureCreate())
				want := provider.Uncertain
				if noSideEffect {
					want = kind
				}
				requireKind(t, err, want)
			})
		}
	}
}

func TestSDKPreflightProfileFailuresPreserveRetryMetadata(t *testing.T) {
	for _, operation := range []string{"describe", "create"} {
		t.Run(operation, func(t *testing.T) {
			mutationFile := filepath.Join(t.TempDir(), "mutation")
			client := newFixtureClient(t, "sdk-preflight-"+operation, provider.Retained, "GRC_MUTATION_FILE="+mutationFile)
			var err error
			if operation == "describe" {
				_, err = client.Describe(t.Context())
			} else {
				describeFixture(t, client)
				_, err = client.Create(t.Context(), fixtureCreate())
			}
			typed := requireKind(t, err, provider.Transient)
			if !typed.NoSideEffect || typed.RetryAfter != 1250*time.Millisecond {
				t.Fatalf("preflight failure lost retry metadata: %+v", typed)
			}
			if _, err := os.Stat(mutationFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("SDK dispatched creation after a preflight failure: %v", err)
			}
			if operation == "describe" {
				_, err = client.Create(t.Context(), fixtureCreate())
				requireKind(t, err, provider.Invalid)
			}
		})
	}
}

func TestPreflightErrorsDoNotRequireSuccessBinding(t *testing.T) {
	for _, mode := range []string{"preflight-without-description", "preflight-mismatched-description"} {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, mode, provider.Retained)
			describeFixture(t, client)
			_, err := client.Create(t.Context(), fixtureCreate())
			typed := requireKind(t, err, provider.Invalid)
			if !typed.NoSideEffect || typed.RetryAfter != 1250*time.Millisecond {
				t.Fatalf("preflight error lost metadata: %+v", typed)
			}
		})
	}
}

func TestSDKOperationFailuresPreserveRetryMetadata(t *testing.T) {
	client := newFixtureClient(t, "sdk-operation-error", provider.Retained)
	describeFixture(t, client)
	for name, operation := range map[string]func() (bool, error){
		"healthy":   func() (bool, error) { return client.Healthy(t.Context(), "resource-1", nil, "owner") },
		"configure": func() (bool, error) { return client.Configure(t.Context(), "resource-1", nil, fixtureCreate().Runner) },
		"drain":     func() (bool, error) { return client.Drain(t.Context(), "resource-1", nil, "owner") },
	} {
		t.Run(name, func(t *testing.T) {
			ready, err := operation()
			typed := requireKind(t, err, provider.Transient)
			if ready || typed.RetryAfter != 1250*time.Millisecond {
				t.Fatalf("operation failure lost retry metadata: ready=%v error=%+v", ready, typed)
			}
		})
	}
}

func TestRequiredOperationResults(t *testing.T) {
	for _, mode := range []string{"confirmation-missing", "confirmation-false", "ready-missing"} {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, mode, provider.Retained)
			describeFixture(t, client)
			if mode == "ready-missing" {
				_, err := client.Healthy(t.Context(), "resource-1", nil, "owner")
				requireKind(t, err, provider.Transient)
			} else {
				requireKind(t, client.Stop(t.Context(), "resource-1"), provider.Transient)
			}
		})
	}
}

func TestDescriptionMustBeVerifiedBeforeOtherOperations(t *testing.T) {
	client := newFixtureClient(t, "never-launch", provider.Retained)
	_, err := client.List(t.Context())
	if !requireKind(t, err, provider.Invalid).NoSideEffect {
		t.Fatal("local rejection must preserve absence of side effects")
	}
	_, err = client.Create(t.Context(), fixtureCreate())
	requireKind(t, err, provider.Invalid)
}

func TestLocalRejectionsDoNotLaunch(t *testing.T) {
	client := newFixtureClient(t, "never-launch", provider.Disposable)
	describeFixture(t, client)
	for _, operation := range []func(context.Context, string) error{client.Resume, client.Persist, client.Stop} {
		requireKind(t, operation(t.Context(), "resource-1"), provider.Invalid)
	}
	client = newFixtureClient(t, "never-launch", provider.Retained)
	describeFixture(t, client)
	requireKind(t, client.Destroy(t.Context(), "resource-1"), provider.Invalid)
	requireKind(t, client.Resume(t.Context(), ""), provider.Invalid)
	_, err := client.Healthy(t.Context(), "resource-1", nil, "")
	requireKind(t, err, provider.Invalid)
	_, err = client.Create(t.Context(), provider.CreateRequest{})
	requireKind(t, err, provider.Invalid)
	_, err = client.Create(t.Context(), provider.CreateRequest{OperationID: "intent", Options: json.RawMessage(`null`)})
	requireKind(t, err, provider.Invalid)
}

func TestCommandEnvironmentAndRequestSecrecy(t *testing.T) {
	t.Setenv("GRC_PARENT_SECRET", "fixture-secret")
	t.Setenv("HOME", "fixture-secret-home")
	t.Setenv("PATH", "fixture-secret-path")
	client := newFixtureClient(t, "secrecy", provider.Retained, "GRC_MAPPED_TOKEN=fixture-secret-explicit", "PATH=/explicit/path", "LANG=en_US.UTF-8")
	describeFixture(t, client)
	_, err := client.Create(t.Context(), fixtureCreate())
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewAndOptionsValidation(t *testing.T) {
	expected := provider.Description{Identity: "fixture", Lifecycle: provider.Retained}
	for name, command := range map[string][]string{"empty": nil, "relative": {"echo"}, "nul": {"/bin/echo", "bad\x00argument"}} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(command, nil, expected, time.Second); err == nil {
				t.Fatal("invalid command was accepted")
			}
		})
	}
	for _, env := range [][]string{{"TOKEN"}, {"=value"}, {"TOKEN=a", "TOKEN=b"}, {"PATH=a", "PATH=b"}, {"bad-name=value"}, {"1TOKEN=value"}, {"TOKEN=bad\x00value"}} {
		if _, err := New([]string{"/does/not/need/to/exist"}, env, expected, time.Second); err == nil {
			t.Fatalf("invalid environment accepted: %q", env)
		}
	}
	client, err := New([]string{"/does/not/need/to/exist"}, nil, expected, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"array":[true,2,"value"]}`)} {
		if err := client.ValidateOptions(options); err != nil {
			t.Fatal(err)
		}
	}
	for _, options := range []string{"null", "[]", "true", "1", "\"value\"", "{", "{} {}", strings.Repeat(" ", provider.MaxMessageBytes+1)} {
		if err := client.ValidateOptions(json.RawMessage(options)); err == nil {
			t.Fatalf("invalid options accepted: %.40q", options)
		}
	}
	_, err = client.Describe(t.Context())
	requireKind(t, err, provider.Transient)
}

func TestSettingsAreImmutable(t *testing.T) {
	client := newFixtureClient(t, "manual", provider.Retained)
	command, env := slices.Clone(client.command), slices.Clone(client.env)
	copyClient, err := New(command, env, provider.Description{Identity: "fixture", Lifecycle: provider.Retained}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	command[0], env[0] = "/not/an/executable", "INVALID=changed"
	describeFixture(t, copyClient)
}

func TestConcurrentRequestsPinScope(t *testing.T) {
	client := newFixtureClient(t, "manual", provider.Retained)
	describeFixture(t, client)
	errors := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := client.List(t.Context())
			errors <- err
		}()
	}
	for range 8 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
}

func TestTimeoutAndOutputOverflowAreBounded(t *testing.T) {
	for _, mode := range []string{"hang", "oversized", "stderr-overflow"} {
		t.Run(mode, func(t *testing.T) {
			client := newFixtureClient(t, mode, provider.Retained)
			describeFixture(t, client)
			client.timeout = 150 * time.Millisecond
			if mode != "hang" {
				client.timeout = 5 * time.Second
			}
			start := time.Now()
			_, err := client.Create(t.Context(), fixtureCreate())
			requireKind(t, err, provider.Uncertain)
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("request cleanup took %s", elapsed)
			}
		})
	}
}

func TestCancellationAndInheritedPipesKillDescendants(t *testing.T) {
	for _, mode := range []string{"descendant", "orphan-pipes"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			client := newFixtureClient(t, mode, provider.Retained, "GRC_CHILD_PID_FILE="+pidFile)
			describeFixture(t, client)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.Create(ctx, fixtureCreate())
				result <- err
			}()
			pid := waitForPID(t, pidFile)
			t.Cleanup(func() {
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					t.Errorf("clean up descendant: %v", err)
				}
			})
			start := time.Now()
			if mode == "descendant" {
				cancel()
			}
			select {
			case err := <-result:
				requireKind(t, err, provider.Uncertain)
			case <-time.After(2 * time.Second):
				t.Fatal("provider did not release inherited pipes")
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("descendant cleanup exceeded its bound")
			}
			waitForExit(t, pid)
		})
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil && pid > 0 {
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descendant did not start")
	return 0
}

func waitForExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descendant remains after request cleanup")
}

// TestCommandProcess runs only in the explicit helper subprocess.
func TestCommandProcess(t *testing.T) {
	if os.Getenv("GRC_COMMAND_HELPER") != "1" {
		return
	}
	if os.Getenv("GRC_HELPER_MODE") == "child" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if err := runFixtureProcess(); err != nil {
		if _, writeErr := fmt.Fprint(os.Stderr, "fixture-secret: ", err); writeErr != nil {
			os.Exit(3)
		}
		os.Exit(2)
	}
	os.Exit(0)
}

func runFixtureProcess() error {
	mode := os.Getenv("GRC_HELPER_MODE")
	if strings.HasPrefix(mode, "sdk") {
		input := io.Reader(os.Stdin)
		instance := fixtureProvider{mutationFile: os.Getenv("GRC_MUTATION_FILE"), operationFailure: mode == "sdk-operation-error"}
		if mode != "sdk" {
			data, err := io.ReadAll(io.LimitReader(os.Stdin, provider.MaxMessageBytes+1))
			if err != nil {
				return err
			}
			var envelope struct {
				Operation string `json:"operation"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				return err
			}
			instance.describeFailure = mode == "sdk-preflight-"+envelope.Operation
			input = bytes.NewReader(data)
		}
		return provider.Serve(context.Background(), input, os.Stdout, func() (provider.Provider, error) { return instance, nil })
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, provider.MaxMessageBytes+1))
	if err != nil {
		return err
	}
	var request provider.Request
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}
	if request.Version != 1 || request.ID == "" || request.Deadline.IsZero() || request.Deadline.After(time.Now().Add(6*time.Second)) {
		return errors.New("request envelope is invalid")
	}
	if request.Expected.Identity != "fixture" || request.Expected.Lifecycle != provider.Lifecycle(os.Getenv("GRC_HELPER_LIFECYCLE")) {
		return errors.New("request omitted configured binding")
	}
	if request.Operation != "describe" && request.Expected.Scope != "account-1" {
		return errors.New("request omitted verified scope")
	}
	if request.Operation == "describe" {
		mode = os.Getenv("GRC_HELPER_DESCRIBE_MODE")
		if mode == "" {
			mode = "manual"
		}
	}
	if mode == "never-launch" {
		return errors.New("operation should have been rejected locally")
	}
	if mode == "hang" {
		time.Sleep(time.Minute)
	}
	if mode == "descendant" || mode == "orphan-pipes" {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		child := exec.Command(executable, "-test.run=^TestCommandProcess$")
		child.Env = []string{"GRC_COMMAND_HELPER=1", "GRC_HELPER_MODE=child", "GORACE=atexit_sleep_ms=0"}
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return err
		}
		if err := os.WriteFile(os.Getenv("GRC_CHILD_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			return err
		}
		if mode == "descendant" {
			return child.Wait()
		}
		return nil
	}
	if mode == "secrecy" {
		if os.Getenv("GRC_PARENT_SECRET") != "" || os.Getenv("HOME") != "" || os.Getenv("GRC_MAPPED_TOKEN") != "fixture-secret-explicit" || os.Getenv("PATH") != "/explicit/path" || os.Getenv("LANG") != "en_US.UTF-8" {
			return errors.New("environment isolation failed")
		}
		if request.Runner == nil || request.Runner.Token != "fixture-secret" || request.OperationID != "intent-1" || string(request.Options) != `{"size":2}` {
			return errors.New("stdin request omitted runner inputs")
		}
		for _, arg := range os.Args {
			if strings.Contains(arg, "fixture-secret") {
				return errors.New("runner token appeared in argv")
			}
		}
	}
	version, id, operation := 1, request.ID, request.Operation
	identity, scope, lifecycle, revision := "fixture", "account-1", os.Getenv("GRC_HELPER_LIFECYCLE"), "fixture-v1"
	absence := lifecycle == string(provider.Disposable)
	switch mode {
	case "version", "preflight-wrong-version":
		version = 2
	case "id", "preflight-wrong-id":
		id = "wrong-id"
	case "operation", "preflight-wrong-operation":
		operation = "wrong-operation"
	case "identity":
		identity = "other-provider"
	case "scope":
		scope = "other-account"
	case "lifecycle":
		lifecycle, absence = "disposable", true
	case "absence":
		absence = !absence
	case "description":
		revision = ""
	case "preflight-without-description", "operation-without-description":
		identity, scope, lifecycle, revision = "", "", "", ""
	case "preflight-mismatched-description":
		identity, scope = "other-provider", "other-account"
	case "revision":
		revision = "fixture-v2"
	case "null":
		_, err := fmt.Fprint(os.Stdout, "null")
		return err
	case "array":
		_, err := fmt.Fprint(os.Stdout, "[]")
		return err
	case "malformed":
		_, err := fmt.Fprint(os.Stdout, "{fixture-secret")
		return err
	case "oversized":
		_, err := os.Stdout.Write(bytes.Repeat([]byte("x"), provider.MaxMessageBytes+1))
		return err
	case "stderr-overflow":
		_, err := os.Stderr.Write(bytes.Repeat([]byte("x"), maxStderrBytes+1))
		return err
	case "nonzero":
		return errors.New("creation response was lost")
	}
	quotedID, err := json.Marshal(id)
	if err != nil {
		return err
	}
	resourceID, state := "resource-1", "running"
	if mode == "id-missing" {
		resourceID = ""
	}
	if mode == "state-invalid" {
		state = "gone"
	}
	resource := fmt.Sprintf(`{"id":%q,"state":%q}`, resourceID, state)
	resources := "[" + resource + "]"
	if mode == "empty" {
		resources = "[]"
	}
	if mode == "null-inventory" {
		resources = "null"
	}
	if mode == "duplicates" {
		resources = "[" + resource + "," + resource + "]"
	}
	fields := fmt.Sprintf(`,"resources":%s`, resources)
	switch request.Operation {
	case "list":
		if mode != "complete-missing" {
			fields += fmt.Sprintf(`,"complete":%t`, mode != "incomplete")
		}
	case "create":
		if mode != "resource-missing" {
			fields += `,"resource":` + resource
		}
		if mode != "create-confirmation-missing" {
			fields += fmt.Sprintf(`,"ok":%t`, mode != "create-confirmation-false")
		}
	case "healthy", "configure", "drain":
		if mode != "ready-missing" {
			fields += fmt.Sprintf(`,"ready":%t`, mode != "not-ready")
		}
	default:
		if mode != "confirmation-missing" {
			fields += fmt.Sprintf(`,"ok":%t`, mode != "confirmation-false")
		}
	}
	if mode == "rejection" || mode == "error-kind" || mode == "retry-negative" || mode == "retry-excessive" || mode == "nonzero-rejection" || mode == "conflicting-result" || strings.HasPrefix(mode, "error-stage-") || strings.HasPrefix(mode, "preflight-") || mode == "operation-without-description" || strings.HasPrefix(mode, "diagnostic-") {
		kind, retry, noSideEffect := os.Getenv("GRC_HELPER_KIND"), int64(1250), os.Getenv("GRC_HELPER_NO_SIDE_EFFECT") == "true"
		stage := "operation"
		if kind == "" {
			kind = "invalid"
		}
		if mode == "error-kind" || mode == "preflight-invalid-kind" {
			kind = "unrecognized"
		}
		if mode == "retry-negative" {
			retry = -1
		}
		if mode == "retry-excessive" || mode == "preflight-invalid-retry" {
			retry = (24 * time.Hour).Milliseconds() + 1
		}
		if mode == "nonzero-rejection" {
			noSideEffect = true
		}
		if strings.HasPrefix(mode, "preflight-") {
			stage, noSideEffect = "preflight", true
		}
		if mode == "preflight-without-guarantee" {
			noSideEffect = false
		}
		if mode == "error-stage-missing" {
			stage = ""
		}
		if mode == "error-stage-unknown" {
			stage = "unknown"
		}
		if mode != "conflicting-result" && mode != "preflight-conflicting-result" {
			fields = `,"resources":null`
		} else {
			noSideEffect = true
		}
		diagnostic := ""
		if raw := os.Getenv("GRC_HELPER_DIAGNOSTIC_JSON"); raw != "" {
			diagnostic = `,"diagnostic":` + raw
		}
		fields += fmt.Sprintf(`,"error":{"kind":%q,"retry_after_ms":%d,"no_side_effect":%t,"stage":%q%s}`, kind, retry, noSideEffect, stage, diagnostic)
	}
	if mode == "unknown-field" {
		fields += `,"detail":"fixture-secret"`
	}
	response := fmt.Sprintf(`{"version":%d,"id":%s,"operation":%q,"description":{"identity":%q,"scope":%q,"lifecycle":%q,"revision":%q,"absence_confirms_destroy":%t}%s}`, version, quotedID, operation, identity, scope, lifecycle, revision, absence, fields)
	if mode == "trailing" {
		response += " {}"
	}
	if _, err := fmt.Fprint(os.Stdout, response); err != nil {
		return err
	}
	if mode == "nonzero-rejection" || mode == "preflight-nonzero" {
		return errors.New("failure after structured rejection")
	}
	return nil
}

type fixtureProvider struct {
	describeFailure  bool
	operationFailure bool
	mutationFile     string
}

func (fixtureProvider) ValidateOptions(json.RawMessage) error { return nil }
func (p fixtureProvider) Describe(context.Context) (provider.Description, error) {
	if p.describeFailure {
		return provider.Description{}, &provider.Error{Kind: provider.Transient, RetryAfter: 1250 * time.Millisecond, Detail: "fixture-secret"}
	}
	lifecycle := provider.Lifecycle(os.Getenv("GRC_HELPER_LIFECYCLE"))
	return provider.Description{Identity: "fixture", Scope: "account-1", Lifecycle: lifecycle, Revision: "fixture-v1", AbsenceConfirmsDestroy: lifecycle == provider.Disposable}, nil
}
func (fixtureProvider) List(context.Context) ([]provider.Resource, error) {
	return []provider.Resource{{ID: "running-1", State: provider.Running}, {ID: "stopped-1", State: provider.Stopped}, {ID: "unknown-1", State: provider.Unknown}}, nil
}
func (p fixtureProvider) Create(_ context.Context, request provider.CreateRequest) (provider.Resource, error) {
	if p.mutationFile != "" {
		if err := os.WriteFile(p.mutationFile, []byte("created"), 0o600); err != nil {
			return provider.Resource{}, err
		}
	}
	if request.OperationID != "intent-1" || request.Runner.Token != "fixture-secret" {
		return provider.Resource{}, errors.New("fixture-secret invalid create payload")
	}
	return provider.Resource{ID: "created-1", State: provider.Running}, nil
}
func (fixtureProvider) Resume(context.Context, string) error  { return nil }
func (fixtureProvider) Persist(context.Context, string) error { return nil }
func (fixtureProvider) Stop(context.Context, string) error    { return nil }
func (fixtureProvider) Destroy(context.Context, string) error { return nil }
func (p fixtureProvider) Healthy(context.Context, string, json.RawMessage, string) (bool, error) {
	return p.readinessResult()
}
func (p fixtureProvider) Configure(context.Context, string, json.RawMessage, provider.RunnerSpec) (bool, error) {
	return p.readinessResult()
}
func (p fixtureProvider) Drain(context.Context, string, json.RawMessage, string) (bool, error) {
	return p.readinessResult()
}
func (p fixtureProvider) readinessResult() (bool, error) {
	if p.operationFailure {
		return false, &provider.Error{Kind: provider.Transient, RetryAfter: 1250 * time.Millisecond, Detail: "fixture-secret"}
	}
	return true, nil
}
