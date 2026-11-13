package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func valid() Config {
	c := Defaults()
	c.ProjectIDs = []int64{1}
	c.GitLabToken = "test-api-token"
	c.WebhookSecret = strings.Repeat("w", 32)
	c.AdminToken = strings.Repeat("a", 32)
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	c.Providers = map[string]Provider{"host": {Identity: "test-account", Lifecycle: provider.Retained, CommandEnv: "HOST_COMMAND", Command: []string{executable}, MaxRunning: 5, Environment: map[string]string{"API_TOKEN": "HOST_TOKEN"}}}
	c.Pools = []Pool{{Name: "linux", Provider: "host", Tags: []string{"linux", "docker"}, MaxInstances: 2, JobsPerInstance: 1, RunnerTokenEnv: "LINUX_TOKEN", Token: "glrt-test-linux", RunnerID: 1001}}
	return c
}

func TestValidationRejectsUnsafeAndAmbiguousConfiguration(t *testing.T) {
	cases := map[string]func(*Config){
		"http URL":                 func(c *Config) { c.GitLabURL = "http://gitlab.example.com" },
		"URL credentials":          func(c *Config) { c.GitLabURL = "https://user:secret@gitlab.example.com" },
		"URL fragment":             func(c *Config) { c.GitLabURL = "https://gitlab.example.com/#secret" },
		"URL query":                func(c *Config) { c.GitLabURL = "https://gitlab.example.com/?token=secret" },
		"invalid port":             func(c *Config) { c.ListenAddr = ":65536" },
		"short secret":             func(c *Config) { c.WebhookSecret = "short" },
		"shared admin secret":      func(c *Config) { c.AdminToken = c.WebhookSecret },
		"newline API token":        func(c *Config) { c.GitLabToken = "secret\nInjected: yes" },
		"duplicate project":        func(c *Config) { c.ProjectIDs = []int64{1, 1} },
		"no pools":                 func(c *Config) { c.Pools = nil },
		"unknown provider":         func(c *Config) { c.Pools[0].Provider = "missing" },
		"capacity zero":            func(c *Config) { p := c.Providers["host"]; p.MaxRunning = 0; c.Providers["host"] = p },
		"duplicate pool":           func(c *Config) { c.Pools = append(c.Pools, c.Pools[0]) },
		"duplicate tag":            func(c *Config) { c.Pools[0].Tags = []string{"linux", "linux"} },
		"empty tags":               func(c *Config) { c.Pools[0].Tags = nil },
		"missing runner ID":        func(c *Config) { c.Pools[0].RunnerID = 0 },
		"invalid protected policy": func(c *Config) { c.Pools[0].ProtectedPendingPolicy = "guess" },
		"probe too short":          func(c *Config) { c.PendingProbeTimeout = Duration(time.Second) },
		"invalid token":            func(c *Config) { c.Pools[0].Token = "registration-token" },
		"invalid env name":         func(c *Config) { c.Pools[0].RunnerTokenEnv = "bad-name" },
		"duplicate adoption":       func(c *Config) { c.Pools[0].AdoptIDs = []string{"same", "same"} },
		"too many adoption IDs":    func(c *Config) { c.Pools[0].AdoptIDs = []string{"a", "b", "c"} },
		"zero concurrency":         func(c *Config) { c.Pools[0].JobsPerInstance = 0 },
		"invalid runtime":          func(c *Config) { c.Runtime = json.RawMessage(`null`) },
		"relative executable":      func(c *Config) { p := c.Providers["host"]; p.Command = []string{"./adapter"}; c.Providers["host"] = p },
		"missing executable": func(c *Config) {
			p := c.Providers["host"]
			p.Command = []string{"/nonexistent/test/adapter"}
			c.Providers["host"] = p
		},
		"missing identity":  func(c *Config) { p := c.Providers["host"]; p.Identity = ""; c.Providers["host"] = p },
		"invalid lifecycle": func(c *Config) { p := c.Providers["host"]; p.Lifecycle = "invalid"; c.Providers["host"] = p },
		"invalid duration":  func(c *Config) { c.MinRefresh = Duration(time.Hour) },
		"short grace":       func(c *Config) { c.IdleGrace = Duration(time.Second) },
		"shared pool token": func(c *Config) {
			p := c.Pools[0]
			p.Name = "other"
			p.RunnerID = 1002
			p.Tags = []string{"other"}
			c.Pools = append(c.Pools, p)
		},
		"same tag set": func(c *Config) {
			p := c.Pools[0]
			p.Name = "other"
			p.RunnerID = 1002
			p.Token = "glrt-other"
			p.Tags = []string{"docker", "linux"}
			c.Pools = append(c.Pools, p)
		},
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid()
			change(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestStrictDecodeAndSecretResolution(t *testing.T) {
	for _, content := range []string{`{"unknown":true}`, `{} {}`, `null`, `{"poll_interval":15}`} {
		cfg := Defaults()
		if err := DecodeStrict([]byte(content), &cfg); err == nil {
			t.Fatalf("accepted %s", content)
		}
	}
	c := valid()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "controller.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"GITLAB_API_TOKEN": c.GitLabToken, "GITLAB_WEBHOOK_SECRET": c.WebhookSecret, "ADMIN_TOKEN": c.AdminToken, "LINUX_TOKEN": c.Pools[0].Token, "PORT": "8088", "STORAGE_DIR": "/state"}
	command, err := json.Marshal(c.Providers["host"].Command)
	if err != nil {
		t.Fatal(err)
	}
	environment["HOST_COMMAND"], environment["HOST_TOKEN"] = string(command), "child-api-secret"
	loaded, err := Load(path, func(name string) string { return environment[name] })
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ListenAddr != ":8088" || loaded.StatePath != "/state/state.json" || loaded.Pools[0].Token != c.Pools[0].Token {
		t.Fatal("environment overrides were not resolved")
	}
	encoded, err := json.Marshal(loaded)
	if err != nil || strings.Contains(string(encoded), "child-api-secret") || len(loaded.Providers["host"].Env) != 1 {
		t.Fatal("child credentials were not resolved privately")
	}
	for _, secret := range []string{c.GitLabToken, c.WebhookSecret, c.AdminToken, c.Pools[0].Token} {
		if strings.Contains(string(data), secret) {
			t.Fatal("secret was serialized")
		}
	}
}
