package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var value string
	if err := json.Unmarshal(b, &value); err != nil {
		return errors.New("duration must be a string such as 30s")
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration: %w", err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

type Provider struct {
	Identity    string             `json:"identity"`
	Lifecycle   provider.Lifecycle `json:"lifecycle"`
	CommandEnv  string             `json:"command_env"`
	Environment map[string]string  `json:"environment"`
	MaxRunning  int                `json:"max_running"`
	Revision    string             `json:"revision"`
	Command     []string           `json:"-"`
	Env         []string           `json:"-"`
}

type Pool struct {
	Name                   string          `json:"name"`
	Provider               string          `json:"provider"`
	Tags                   []string        `json:"tags"`
	MaxInstances           int             `json:"max_instances"`
	JobsPerInstance        int             `json:"jobs_per_instance"`
	Priority               int             `json:"priority"`
	RunnerTokenEnv         string          `json:"runner_token_env"`
	RunnerID               int64           `json:"runner_id"`
	ProtectedPendingPolicy string          `json:"protected_pending_policy,omitempty"`
	ProtectedProbeMaxHosts int             `json:"protected_probe_max_hosts,omitempty"`
	AdoptIDs               []string        `json:"adopt_ids,omitempty"`
	Options                json.RawMessage `json:"options"`
	Token                  string          `json:"-"`
}

type Config struct {
	ControllerID        string              `json:"controller_id"`
	ListenAddr          string              `json:"listen_addr"`
	StatePath           string              `json:"state_path"`
	GitLabURL           string              `json:"gitlab_url"`
	ProjectIDs          []int64             `json:"project_ids"`
	PollInterval        Duration            `json:"poll_interval"`
	JobsPollInterval    Duration            `json:"jobs_poll_interval"`
	MinRefresh          Duration            `json:"min_refresh_interval"`
	IdleGrace           Duration            `json:"idle_grace"`
	ObservationGrace    Duration            `json:"observation_grace"`
	RequestTimeout      Duration            `json:"request_timeout"`
	ReadinessTimeout    Duration            `json:"readiness_timeout"`
	PendingProbeTimeout Duration            `json:"pending_probe_timeout,omitempty"`
	Providers           map[string]Provider `json:"providers"`
	Pools               []Pool              `json:"pools"`
	Runtime             json.RawMessage     `json:"runner"`
	GitLabToken         string              `json:"-"`
	WebhookSecret       string              `json:"-"`
	AdminToken          string              `json:"-"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func Defaults() Config {
	return Config{
		ControllerID: "gitlab-runner-controller", ListenAddr: ":3000", StatePath: "data/state.json",
		GitLabURL: "https://gitlab.com", PollInterval: Duration(15 * time.Second),
		JobsPollInterval: Duration(time.Minute), MinRefresh: Duration(5 * time.Second),
		IdleGrace: Duration(45 * time.Second), ObservationGrace: Duration(90 * time.Second),
		RequestTimeout:   Duration(50 * time.Second),
		ReadinessTimeout: Duration(5 * time.Minute),
		Runtime:          json.RawMessage(`{}`),
	}
}

func DecodeStrict(data []byte, into any) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("JSON null is not a configuration object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected a single JSON value")
	}
	return nil
}

func Load(path string, getenv func(string) string) (Config, error) {
	cfg := Defaults()
	data := []byte(getenv("CONTROLLER_CONFIG_JSON"))
	if len(data) == 0 {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read configuration: %w", err)
		}
	}
	if len(data) > 1<<20 {
		return cfg, errors.New("configuration exceeds 1 MiB")
	}
	if err := DecodeStrict(data, &cfg); err != nil {
		return cfg, fmt.Errorf("decode configuration: %w", err)
	}
	if port := getenv("PORT"); port != "" {
		cfg.ListenAddr = ":" + port
	}
	if dir := getenv("STORAGE_DIR"); dir != "" {
		cfg.StatePath = filepath.Join(dir, "state.json")
	}
	cfg.GitLabURL = strings.TrimRight(cfg.GitLabURL, "/")
	cfg.GitLabToken = getenv("GITLAB_API_TOKEN")
	cfg.WebhookSecret = getenv("GITLAB_WEBHOOK_SECRET")
	cfg.AdminToken = getenv("ADMIN_TOKEN")
	for i := range cfg.Pools {
		cfg.Pools[i].Token = getenv(cfg.Pools[i].RunnerTokenEnv)
	}
	for name, spec := range cfg.Providers {
		if err := json.Unmarshal([]byte(getenv(spec.CommandEnv)), &spec.Command); err != nil {
			return cfg, fmt.Errorf("provider %s command_env must reference a JSON argument array", name)
		}
		keys := make([]string, 0, len(spec.Environment))
		for key := range spec.Environment {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			source := spec.Environment[key]
			if !envName.MatchString(key) || !envName.MatchString(source) {
				return cfg, fmt.Errorf("provider %s has invalid environment bindings", name)
			}
			value := getenv(source)
			if value == "" || strings.ContainsRune(value, '\x00') {
				return cfg, fmt.Errorf("provider %s requires environment variable %s", name, source)
			}
			spec.Env = append(spec.Env, key+"="+value)
		}
		cfg.Providers[name] = spec
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func HTTPSURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	return nil
}

func (c Config) Validate() error {
	if !identifier.MatchString(c.ControllerID) {
		return errors.New("controller_id must be a short alphanumeric identifier")
	}
	_, port, err := net.SplitHostPort(c.ListenAddr)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("listen_addr must contain a valid port between 1 and 65535")
	}
	if c.StatePath == "" {
		return errors.New("state_path is required")
	}
	if err := HTTPSURL(c.GitLabURL); err != nil {
		return fmt.Errorf("gitlab_url: %w", err)
	}
	if c.GitLabToken == "" || strings.ContainsAny(c.GitLabToken, "\r\n") {
		return errors.New("GITLAB_API_TOKEN is required and must not contain newlines")
	}
	if len(c.WebhookSecret) < 32 || strings.ContainsAny(c.WebhookSecret, "\r\n") {
		return errors.New("GITLAB_WEBHOOK_SECRET must contain at least 32 characters and no newlines")
	}
	if c.AdminToken != "" && (len(c.AdminToken) < 32 || c.AdminToken == c.WebhookSecret || strings.ContainsAny(c.AdminToken, "\r\n")) {
		return errors.New("ADMIN_TOKEN must contain at least 32 characters and differ from the webhook secret")
	}
	if len(c.ProjectIDs) == 0 || len(c.ProjectIDs) > 100 {
		return errors.New("project_ids must contain between 1 and 100 numeric IDs")
	}
	seenProjects := make(map[int64]bool)
	for _, id := range c.ProjectIDs {
		if id <= 0 || seenProjects[id] {
			return errors.New("project_ids must be positive and unique")
		}
		seenProjects[id] = true
	}
	for _, value := range []Duration{c.PollInterval, c.JobsPollInterval, c.MinRefresh, c.IdleGrace, c.ObservationGrace, c.RequestTimeout, c.ReadinessTimeout} {
		if value < Duration(time.Second) || value > Duration(24*time.Hour) {
			return errors.New("durations must be between 1s and 24h")
		}
	}
	if c.MinRefresh > c.JobsPollInterval || c.IdleGrace < c.PollInterval || c.ObservationGrace < c.PollInterval || c.RequestTimeout > Duration(5*time.Minute) {
		return errors.New("invalid refresh, idle, observation, or request timeout relationship")
	}
	if len(c.Pools) == 0 || len(c.Pools) > 100 {
		return errors.New("pools must contain between 1 and 100 entries")
	}
	for name, p := range c.Providers {
		if !identifier.MatchString(name) || p.Identity == "" || len(p.Identity) > 256 || strings.ContainsAny(p.Identity, "\r\n\x00") || (p.Lifecycle != provider.Retained && p.Lifecycle != provider.Disposable) || p.MaxRunning < 1 || p.MaxRunning > 1000 || !envName.MatchString(p.CommandEnv) {
			return errors.New("each provider needs a valid name, identity, lifecycle, command_env, and max_running between 1 and 1000")
		}
		if len(p.Command) == 0 || len(p.Command) > 128 || !filepath.IsAbs(p.Command[0]) {
			return fmt.Errorf("provider %s command must be a JSON argument array with an absolute executable path", name)
		}
		for _, arg := range p.Command {
			if len(arg) > 64<<10 || strings.ContainsRune(arg, '\x00') {
				return fmt.Errorf("provider %s has an invalid command argument", name)
			}
		}
		info, err := os.Stat(p.Command[0])
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("provider %s executable is unavailable or not executable", name)
		}
	}
	seenNames, seenTokens, seenIDs, seenTags := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	seenRunners := map[int64]bool{}
	total := 0
	for _, p := range c.Pools {
		if p.RunnerID <= 0 || seenRunners[p.RunnerID] {
			return errors.New("each pool requires a distinct positive runner_id")
		}
		seenRunners[p.RunnerID] = true
		if p.ProtectedPendingPolicy != "" && p.ProtectedPendingPolicy != "probe_once" && p.ProtectedPendingPolicy != "strict" {
			return errors.New("protected_pending_policy must be probe_once or strict")
		}
		if p.ProtectedProbeMaxHosts < 0 || p.ProtectedProbeMaxHosts > 10 {
			return errors.New("protected_probe_max_hosts must be between 1 and 10 when set")
		}
		if !identifier.MatchString(p.Name) || seenNames[p.Name] {
			return errors.New("pool names must be valid and unique")
		}
		seenNames[p.Name] = true
		if _, ok := c.Providers[p.Provider]; !ok {
			return fmt.Errorf("pool %s references an unknown provider", p.Name)
		}
		if p.MaxInstances < 1 || p.MaxInstances > 100 || p.JobsPerInstance < 1 || p.JobsPerInstance > 64 || p.Priority < -10000 || p.Priority > 10000 {
			return fmt.Errorf("pool %s has invalid instance, concurrency, or priority limits", p.Name)
		}
		total += p.MaxInstances
		if len(p.Tags) == 0 || len(p.Tags) > 100 {
			return fmt.Errorf("pool %s requires between 1 and 100 runner tags", p.Name)
		}
		tags := slices.Clone(p.Tags)
		slices.Sort(tags)
		for i, tag := range tags {
			if strings.TrimSpace(tag) == "" || len(tag) > 255 || strings.ContainsAny(tag, "\r\n,") || (i > 0 && tags[i-1] == tag) {
				return fmt.Errorf("pool %s has invalid or duplicate tags", p.Name)
			}
		}
		key, _ := json.Marshal(tags)
		if seenTags[string(key)] {
			return errors.New("pool tag sets must be unique")
		}
		seenTags[string(key)] = true
		if !envName.MatchString(p.RunnerTokenEnv) || !strings.HasPrefix(p.Token, "glrt-") || len(p.Token) <= 5 || strings.ContainsAny(p.Token, "\r\n") || seenTokens[p.Token] {
			return fmt.Errorf("pool %s needs a unique glrt- token in runner_token_env", p.Name)
		}
		seenTokens[p.Token] = true
		if len(p.AdoptIDs) > p.MaxInstances {
			return fmt.Errorf("pool %s has more adoption IDs than instances", p.Name)
		}
		for _, id := range p.AdoptIDs {
			key := p.Provider + "/" + id
			if strings.TrimSpace(id) == "" || seenIDs[key] {
				return errors.New("adoption IDs must be nonempty and unique within each provider")
			}
			seenIDs[key] = true
		}
	}
	if total > 1000 {
		return errors.New("total configured instances must not exceed 1000")
	}
	if c.PendingProbeTimeout != 0 && (c.PendingProbeTimeout < c.JobsPollInterval || c.PendingProbeTimeout > Duration(24*time.Hour)) {
		return errors.New("pending_probe_timeout must cover at least one jobs_poll_interval and be at most 24h")
	}
	var runtime map[string]json.RawMessage
	if err := json.Unmarshal(c.Runtime, &runtime); err != nil || runtime == nil {
		return errors.New("runner must be a JSON object of adapter-defined options")
	}
	return nil
}

func (p Pool) ProbeMaxHosts() int {
	if p.ProtectedProbeMaxHosts > 0 {
		return p.ProtectedProbeMaxHosts
	}
	return 2
}

func (c Config) ProbeTimeout() time.Duration {
	if c.PendingProbeTimeout > 0 {
		return time.Duration(c.PendingProbeTimeout)
	}
	return max(time.Duration(c.ObservationGrace), 2*time.Duration(c.JobsPollInterval))
}
