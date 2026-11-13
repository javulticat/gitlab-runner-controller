// Testprovider is a non-provisioning fixture for container and protocol checks.
package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type fixture struct{}

func (fixture) Describe(context.Context) (provider.Description, error) {
	return provider.Description{Identity: os.Getenv("ACCOUNT_ID"), Scope: "fixture-scope", Lifecycle: provider.Lifecycle(os.Getenv("LIFECYCLE")), Revision: "1", AbsenceConfirmsDestroy: true}, nil
}
func (fixture) ValidateOptions(json.RawMessage) error             { return nil }
func (fixture) List(context.Context) ([]provider.Resource, error) { return []provider.Resource{}, nil }
func rejected() error {
	return &provider.Error{Kind: provider.Invalid, Detail: "fixture does not provision resources", NoSideEffect: true}
}
func (fixture) Create(context.Context, provider.CreateRequest) (provider.Resource, error) {
	return provider.Resource{}, rejected()
}
func (fixture) Resume(context.Context, string) error  { return rejected() }
func (fixture) Persist(context.Context, string) error { return rejected() }
func (fixture) Stop(context.Context, string) error    { return rejected() }
func (fixture) Destroy(context.Context, string) error { return rejected() }
func (fixture) Healthy(context.Context, string, json.RawMessage, string) (bool, error) {
	return false, rejected()
}
func (fixture) Drain(context.Context, string, json.RawMessage, string) (bool, error) {
	return false, rejected()
}
func (fixture) Configure(context.Context, string, json.RawMessage, provider.RunnerSpec) (bool, error) {
	return false, rejected()
}

func main() {
	if err := provider.Serve(context.Background(), os.Stdin, os.Stdout, func() (provider.Provider, error) { return fixture{}, nil }); err != nil {
		os.Exit(1)
	}
}
