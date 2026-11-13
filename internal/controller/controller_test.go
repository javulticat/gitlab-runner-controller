package controller

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/config"
	"github.com/javulticat/gitlab-runner-controller/internal/gitlab"
	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

type fakeJobs struct {
	jobs        []state.Job
	err         error
	calls       int
	onSync      func()
	policies    map[int64]gitlab.RunnerPolicy
	policiesErr error
	policyCalls int
}

func (f *fakeJobs) Sync(context.Context, []int64, []state.Job) ([]state.Job, error) {
	f.calls++
	if f.onSync != nil {
		f.onSync()
	}
	return slices.Clone(f.jobs), f.err
}

func (f *fakeJobs) Policies(context.Context, []int64, []int64) (map[int64]gitlab.RunnerPolicy, error) {
	f.policyCalls++
	policies := maps.Clone(f.policies)
	for id, policy := range policies {
		policy.Tags = slices.Clone(policy.Tags)
		policy.Projects = maps.Clone(policy.Projects)
		policies[id] = policy
	}
	return policies, f.policiesErr
}

type fakeProvider struct {
	resources                                                                         map[string]provider.Resource
	description                                                                       provider.Description
	describes, creates, resumes, persists, configures, drains, stops, destroys, lists int
	describeErr                                                                       error
	createErr                                                                         error
	configureErr                                                                      error
	configurePending                                                                  bool
	listErr                                                                           error
	destroyErr                                                                        error
	createState                                                                       provider.PowerState
	drained                                                                           bool
	stopVisible                                                                       bool
	destroyVisible                                                                    bool
	specs                                                                             []provider.RunnerSpec
	onCreate                                                                          func()
	onStop                                                                            func()
	onDestroy                                                                         func()
}

func (*fakeProvider) ValidateOptions(json.RawMessage) error { return nil }
func (p *fakeProvider) Describe(context.Context) (provider.Description, error) {
	p.describes++
	return p.description, p.describeErr
}
func (p *fakeProvider) List(context.Context) ([]provider.Resource, error) {
	p.lists++
	if p.listErr != nil {
		return nil, p.listErr
	}
	result := []provider.Resource{}
	for _, r := range p.resources {
		result = append(result, r)
	}
	return result, nil
}
func (p *fakeProvider) Create(_ context.Context, request provider.CreateRequest) (provider.Resource, error) {
	p.creates++
	p.specs = append(p.specs, request.Runner)
	if p.onCreate != nil {
		p.onCreate()
	}
	if p.createErr != nil {
		return provider.Resource{}, p.createErr
	}
	id := request.OperationID
	power := p.createState
	if power == "" {
		power = provider.Running
	}
	r := provider.Resource{ID: id, State: power}
	p.resources[id] = r
	return r, nil
}
func (p *fakeProvider) Resume(_ context.Context, id string) error {
	p.resumes++
	p.resources[id] = provider.Resource{ID: id, State: provider.Running}
	return nil
}
func (p *fakeProvider) Persist(context.Context, string) error { p.persists++; return nil }
func (p *fakeProvider) Healthy(context.Context, string, json.RawMessage, string) (bool, error) {
	return true, nil
}
func (p *fakeProvider) Configure(_ context.Context, _ string, _ json.RawMessage, spec provider.RunnerSpec) (bool, error) {
	p.configures++
	p.specs = append(p.specs, spec)
	return !p.configurePending && p.configureErr == nil, p.configureErr
}
func (p *fakeProvider) Drain(context.Context, string, json.RawMessage, string) (bool, error) {
	p.drains++
	return p.drained, nil
}
func (p *fakeProvider) Stop(_ context.Context, id string) error {
	p.stops++
	if p.onStop != nil {
		p.onStop()
	}
	if p.stopVisible {
		p.resources[id] = provider.Resource{ID: id, State: provider.Stopped}
	}
	return nil
}

func (p *fakeProvider) Destroy(_ context.Context, id string) error {
	p.destroys++
	if p.onDestroy != nil {
		p.onDestroy()
	}
	if p.destroyVisible {
		delete(p.resources, id)
	}
	return p.destroyErr
}

func testProvider(lifecycle provider.Lifecycle, maximum int) config.Provider {
	return config.Provider{Identity: "account-test", Lifecycle: lifecycle, Revision: "1", MaxRunning: maximum}
}

func newFakeProvider(lifecycle provider.Lifecycle) *fakeProvider {
	return &fakeProvider{
		resources: map[string]provider.Resource{}, drained: true, stopVisible: true,
		description: provider.Description{Identity: "account-test", Scope: "test-scope", Lifecycle: lifecycle,
			Revision: "1", AbsenceConfirmsDestroy: lifecycle == provider.Disposable},
	}
}

type fixture struct {
	c         *Controller
	cfg       config.Config
	store     *state.Store
	api       *fakeJobs
	host      *fakeProvider
	providers map[string]provider.Provider
	now       time.Time
	path      string
}

func testPool(name string, maximum, jobs, priority int) config.Pool {
	return config.Pool{Name: name, Provider: "host", RunnerID: poolRunnerID(name), Tags: []string{name}, MaxInstances: maximum,
		JobsPerInstance: jobs, Priority: priority, RunnerTokenEnv: "TOKEN_" + name, Token: "glrt-test-" + name}
}

func poolRunnerID(name string) int64 {
	hash := fnv.New64a()
	// Hash writes cannot fail.
	_, _ = hash.Write([]byte(name))
	return int64(hash.Sum64()>>2) + 1
}

func setup(t *testing.T, pools ...config.Pool) *fixture {
	t.Helper()
	return setupLifecycle(t, provider.Retained, pools...)
}

func setupLifecycle(t *testing.T, lifecycle provider.Lifecycle, pools ...config.Pool) *fixture {
	t.Helper()
	cfg := config.Defaults()
	cfg.ProjectIDs = []int64{1}
	cfg.Pools = pools
	cfg.Providers = map[string]config.Provider{"host": testProvider(lifecycle, 5)}
	f := &fixture{cfg: cfg, api: &fakeJobs{}, host: newFakeProvider(lifecycle), now: time.Unix(1_800_000_000, 0), path: filepath.Join(t.TempDir(), "data", "state.json")}
	f.providers = map[string]provider.Provider{"host": f.host}
	for _, pool := range pools {
		for _, id := range pool.AdoptIDs {
			f.host.resources[id] = provider.Resource{ID: id, State: provider.Running}
		}
	}
	store, err := state.Open(f.path, cfg.ControllerID, Binding(cfg))
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	t.Cleanup(func() {
		if err := f.store.Close(); err != nil {
			t.Error(err)
		}
	})
	f.construct(t)
	return f
}

func (f *fixture) construct(t *testing.T) {
	t.Helper()
	if f.api.policies == nil {
		f.api.policies = map[int64]gitlab.RunnerPolicy{}
	}
	for _, pool := range f.cfg.Pools {
		if _, exists := f.api.policies[pool.RunnerID]; !exists {
			f.api.policies[pool.RunnerID] = gitlab.RunnerPolicy{ID: pool.RunnerID, Known: true,
				Tags: slices.Clone(pool.Tags), Projects: map[int64]bool{1: true}}
		}
	}
	c, err := New(f.cfg, f.store, f.api, f.providers, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	f.c = c
	f.c.now = func() time.Time { return f.now }
}

func (f *fixture) restart(t *testing.T) {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(f.path, f.cfg.ControllerID, Binding(f.cfg))
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	f.construct(t)
}

func (f *fixture) step(t *testing.T, advance time.Duration) {
	t.Helper()
	f.now = f.now.Add(advance)
	if err := f.c.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func job(id int64, status, tag string) state.Job {
	runnerID := int64(0)
	if status != "pending" {
		runnerID = poolRunnerID(tag)
	}
	return state.Job{ProjectID: 1, ID: id, Status: status, Tags: []string{tag}, RunnerID: runnerID}
}

func TestPriorityCapacityAndGenericRunnerSpec(t *testing.T) {
	for _, first := range []string{"general", "protected"} {
		t.Run(first, func(t *testing.T) {
			general, protected := testPool("general", 4, 1, 0), testPool("protected", 1, 1, 0)
			if first == "general" {
				general.Priority = 100
			} else {
				protected.Priority = 100
			}
			f := setup(t, general, protected)
			f.cfg.GitLabURL = "https://gitlab.example.com/subpath"
			f.c.cfg.GitLabURL = f.cfg.GitLabURL
			f.host.resources["unmanaged"] = provider.Resource{ID: "unmanaged", State: provider.Running}
			for i := range 4 {
				f.api.jobs = append(f.api.jobs, job(int64(i+1), "pending", "general"))
			}
			f.api.jobs = append(f.api.jobs, job(9, "pending", "protected"))
			f.host.onCreate = func() {
				found := false
				for _, pool := range f.store.Snapshot().Pools {
					for _, slot := range pool.Slots {
						if slot.Operation != nil && slot.Operation.Kind == "create" {
							found = true
						}
					}
				}
				if !found {
					t.Error("creation did not have a durable intent")
				}
			}
			f.step(t, 0)
			if f.host.creates != 4 {
				t.Fatalf("created %d, want 4", f.host.creates)
			}
			if f.host.specs[0].Name != f.cfg.ControllerID+"/"+first+"/0" {
				t.Fatal("priority was ignored")
			}
			if f.host.specs[0].GitLabURL != f.cfg.GitLabURL {
				t.Fatal("runner registration URL was hardcoded")
			}
			if f.host.stops != 0 {
				t.Fatal("unmanaged resource was stopped")
			}
		})
	}
}

func TestJobsPerInstanceAndPersistentRestart(t *testing.T) {
	f := setup(t, testPool("linux", 4, 3, 0))
	for i := range 7 {
		f.api.jobs = append(f.api.jobs, job(int64(i+1), "pending", "linux"))
	}
	f.step(t, 0)
	if f.host.creates != 3 {
		t.Fatalf("created %d, want 3", f.host.creates)
	}
	f.step(t, 15*time.Second)
	if f.host.persists != 3 || f.host.configures != 3 || f.host.specs[0].Jobs != 3 {
		t.Fatal("instances were not persisted and configured")
	}
	f.restart(t)
	f.step(t, time.Minute)
	if f.host.creates != 3 {
		t.Fatal("restart duplicated resources")
	}
}

func TestBusyPoolNeverScalesInAndRunningJobsDoNotWakeStoppedInstances(t *testing.T) {
	p := testPool("linux", 3, 1, 0)
	p.AdoptIDs = []string{"a", "b", "c"}
	f := setup(t, p)
	f.host.resources["c"] = provider.Resource{ID: "c", State: provider.Stopped}
	f.api.jobs = []state.Job{job(1, "running", "linux")}
	f.step(t, 0)
	f.step(t, 2*time.Minute)
	if f.host.stops != 0 || f.host.resumes != 0 {
		t.Fatal("running jobs changed pool capacity")
	}
}

func TestIdleDrainAndObservedStopBeforeCapacityReuse(t *testing.T) {
	p := testPool("linux", 1, 1, 0)
	p.AdoptIDs = []string{"a"}
	f := setup(t, p)
	f.host.drained = false
	f.host.stopVisible = false
	f.step(t, 0)
	f.step(t, time.Minute)
	if f.host.stops != 0 || f.host.drains != 1 {
		t.Fatal("resource stopped before runner drained")
	}
	f.host.drained = true
	f.host.onStop = func() {
		slot := f.store.Snapshot().Pools["linux"].Slots[0]
		if slot.Operation == nil || slot.Operation.Kind != "stop" || slot.Operation.Phase != "requested" {
			t.Error("stop had no durable intent")
		}
	}
	f.step(t, 15*time.Second)
	if f.host.stops != 1 {
		t.Fatal("drained resource was not stopped")
	}
	f.step(t, 15*time.Second)
	if f.host.stops != 1 || f.store.Snapshot().Pools["linux"].Slots[0].Operation == nil {
		t.Fatal("stale inventory freed capacity or repeated stop too soon")
	}
	f.host.resources["a"] = provider.Resource{ID: "a", State: provider.Stopped}
	f.step(t, 15*time.Second)
	if f.store.Snapshot().Pools["linux"].Slots[0].Operation != nil {
		t.Fatal("stop confirmation was not recorded")
	}
}

func TestReclaimWaitsForLaterProviderObservation(t *testing.T) {
	p := testPool("idle", 1, 1, 0)
	p.AdoptIDs = []string{"a"}
	f := setup(t, p, testPool("urgent", 1, 1, 10))
	f.c.cfg.Providers["host"] = testProvider(provider.Retained, 1)
	f.api.jobs = []state.Job{job(1, "pending", "urgent")}
	f.step(t, 0)
	if f.host.stops != 1 || f.host.creates != 0 {
		t.Fatal("capacity was reused before inventory confirmed stop")
	}
	f.step(t, 15*time.Second)
	if f.host.creates != 1 {
		t.Fatal("confirmed free capacity was not allocated")
	}
}

func TestWebhookCannotOverwriteVerifiedJobOrRaceLifecycle(t *testing.T) {
	p := testPool("linux", 1, 1, 0)
	p.AdoptIDs = []string{"a"}
	f := setup(t, p)
	f.api.jobs = []state.Job{job(1, "running", "linux")}
	f.step(t, 0)
	if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if f.store.Snapshot().Jobs["1/1"].Status != "running" {
		t.Fatal("unverified hook replaced authoritative status")
	}
	f.api.jobs = nil
	f.api.onSync = func() {
		if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "failed"}); err != nil {
			t.Error(err)
		}
	}
	f.now = f.now.Add(time.Minute)
	err := f.c.Step(context.Background())
	if err != nil {
		t.Fatalf("racing synchronization should defer reconciliation: %v", err)
	}
	if f.host.stops != 0 || f.store.Snapshot().Jobs["1/1"].Status != "running" {
		t.Fatal("racing webhook allowed an unsafe lifecycle decision")
	}
}

func TestRateLimitBackoffIsDurableAndBlocksMutations(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.api.err = &gitlab.RateLimitError{Delay: 2 * time.Minute}
	if err := f.c.Step(context.Background()); err == nil {
		t.Fatal("rate limit was ignored")
	}
	until := f.store.Snapshot().BackoffUntil
	if !until.Equal(f.now.Add(2*time.Minute)) || f.host.lists != 0 {
		t.Fatal("rate limit did not stop lifecycle work")
	}
	f.restart(t)
	if err := f.c.Step(context.Background()); err == nil || f.api.calls != 1 {
		t.Fatal("restart bypassed backoff")
	}
	f.api.err = nil
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, 2*time.Minute)
	if f.host.creates != 1 || !f.store.Snapshot().BackoffUntil.IsZero() {
		t.Fatal("backoff did not recover")
	}
}

func TestRefreshSpacingAndDuplicateHints(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.step(t, 0)
	hint := state.Hint{ProjectID: 1, ID: 1, Status: "pending"}
	if err := f.c.Hint(hint); err != nil {
		t.Fatal(err)
	}
	revision := f.store.Snapshot().HintRevision
	if err := f.c.Hint(hint); err != nil {
		t.Fatal(err)
	}
	if f.store.Snapshot().HintRevision != revision {
		t.Fatal("duplicate hook changed hint revision")
	}
	if err := f.c.Step(context.Background()); err != nil || f.api.calls != 1 {
		t.Fatal("minimum refresh spacing was bypassed")
	}
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, 5*time.Second)
	if f.host.creates != 1 {
		t.Fatal("verified demand did not allocate capacity")
	}
}

func TestUncertainCreationIsNotReplayedAndCanBeResolved(t *testing.T) {
	f := setup(t, testPool("linux", 2, 1, 0))
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.host.createErr = &provider.Error{Operation: "create", Kind: provider.Uncertain, Detail: "response lost"}
	f.step(t, 0)
	if f.store.Snapshot().Pools["linux"].Slots[0].Operation.Kind != "unresolved" {
		t.Fatal("uncertain creation was not retained")
	}
	f.restart(t)
	f.step(t, time.Minute)
	if f.host.creates != 1 {
		t.Fatal("uncertain creation was replayed")
	}
	if f.c.Status().Ready {
		t.Fatal("unresolved creation reported ready")
	}
	if err := f.c.ResolveCreate("linux", 0, "recovered", false); err != nil {
		t.Fatal(err)
	}
	f.host.createErr = nil
	f.host.resources["recovered"] = provider.Resource{ID: "recovered", State: provider.Running}
	f.step(t, 15*time.Second)
	if f.host.creates != 1 || !f.store.Snapshot().Pools["linux"].Slots[0].Ready {
		t.Fatal("recovered creation was not adopted")
	}
}

func TestInterruptedCreationAndMissingResourceKeepReservations(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	f.step(t, 0)
	if err := f.c.begin("linux", 0, "create"); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, time.Minute)
	if f.host.creates != 0 {
		t.Fatal("interrupted creation was replayed")
	}
	if err := f.c.ResolveCreate("linux", 0, "", true); err != nil {
		t.Fatal(err)
	}
	f.step(t, 15*time.Second)
	if f.host.creates != 1 {
		t.Fatal("explicit absence resolution did not release reservation")
	}
	f.host.resources = map[string]provider.Resource{}
	f.step(t, 15*time.Second)
	if f.host.creates != 1 || f.store.Snapshot().Pools["linux"].Slots[0].ResourceID == "" {
		t.Fatal("missing inventory was treated as permission to replace")
	}
}

func TestStorageFailurePreventsExternalWork(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	dir := filepath.Dir(f.path)
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	if err := f.c.Step(context.Background()); !errors.Is(err, state.ErrPoisoned) {
		t.Fatalf("storage failure: %v", err)
	}
	if f.api.calls != 0 || f.host.lists != 0 || f.host.creates != 0 {
		t.Fatal("external work continued after persistence failed")
	}
}

func TestAmbiguousAndUnsupportedJobTagsCannotAllocate(t *testing.T) {
	a, b := testPool("a", 1, 1, 0), testPool("b", 1, 1, 0)
	a.Tags, b.Tags = []string{"shared", "a"}, []string{"shared", "b"}
	f := setup(t, a, b)
	f.api.jobs = []state.Job{job(1, "pending", "shared"), job(2, "pending", "unsupported"), {ProjectID: 1, ID: 3, Status: "pending"}}
	f.step(t, 0)
	if f.host.creates != 0 {
		t.Fatal("ambiguous or incompatible job allocated a runner")
	}
	if compatible([]string{"a", "unsupported"}, a.Tags) {
		t.Fatal("partial tag match was accepted")
	}
}

func TestAmbiguousPendingJobsProtectWithoutIncreasingScaleDemand(t *testing.T) {
	a, b := testPool("a", 5, 1, 0), testPool("b", 5, 1, 0)
	a.Tags, b.Tags = []string{"shared", "a"}, []string{"shared", "b"}
	f := setup(t, a, b)
	f.api.jobs = []state.Job{job(1, "pending", "a")}
	for i := range 20 {
		f.api.jobs = append(f.api.jobs, job(int64(i+2), "pending", "shared"))
	}
	f.step(t, 0)
	if f.host.creates != 1 {
		t.Fatalf("ambiguous jobs increased scale demand: %d", f.host.creates)
	}
	if !f.store.Snapshot().Pools["b"].IdleSince.IsZero() {
		t.Fatal("ambiguous job did not protect the compatible idle pool")
	}
}

func TestProviderRetryAfterIsDurableAndAppliesToEntireAccount(t *testing.T) {
	for _, source := range []string{"inventory", "configure"} {
		t.Run(source, func(t *testing.T) {
			p := testPool("linux", 2, 1, 0)
			p.AdoptIDs = []string{"a", "b"}
			f := setup(t, p)
			limited := &provider.Error{Operation: source, Kind: provider.Transient, Detail: "rate limited", RetryAfter: 2 * time.Minute}
			if source == "inventory" {
				f.host.listErr = limited
			} else {
				f.host.configureErr = limited
			}
			if err := f.c.Step(context.Background()); source == "inventory" && err == nil {
				t.Fatal("inventory rate limit was not reported")
			}
			if !f.store.Snapshot().ProviderBackoff["host"].Equal(f.now.Add(2 * time.Minute)) {
				t.Fatal("provider Retry-After was not persisted")
			}
			if source == "configure" && f.host.configures != 1 {
				t.Fatal("other slots ignored the account rate limit")
			}
			lists, configured := f.host.lists, f.host.configures
			f.restart(t)
			f.now = f.now.Add(15 * time.Second)
			if err := f.c.Step(context.Background()); err == nil {
				t.Fatal("restart bypassed account backoff")
			}
			if f.host.lists != lists || f.host.configures != configured {
				t.Fatal("provider calls occurred during Retry-After")
			}
		})
	}
}

func TestShrinkRetainsExtraSlotsAndRemovalRefusesActiveResources(t *testing.T) {
	p := testPool("linux", 2, 1, 0)
	p.AdoptIDs = []string{"a", "b"}
	f := setup(t, p)
	f.api.jobs = []state.Job{job(1, "running", "linux")}
	f.step(t, 0)
	f.cfg.Pools[0].MaxInstances = 1
	f.cfg.Pools[0].AdoptIDs = nil
	f.construct(t)
	f.step(t, time.Minute)
	if len(f.store.Snapshot().Pools["linux"].Slots) != 2 || f.host.stops != 0 {
		t.Fatal("shrinking discarded or stopped a busy slot")
	}
	f.cfg.Pools = []config.Pool{testPool("other", 1, 1, 0)}
	if _, err := New(f.cfg, f.store, f.api, map[string]provider.Provider{"host": f.host}, f.c.log); err == nil {
		t.Fatal("active pool removal was accepted")
	}
}

func TestConcurrentHintsAreDurableAndTokenFreeStatus(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	var workers sync.WaitGroup
	for i := range 20 {
		workers.Go(func() {
			if err := f.c.Hint(state.Hint{ProjectID: 1, ID: int64(i + 1), Status: "pending"}); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if len(f.store.Snapshot().Hints) != 20 {
		t.Fatal("concurrent hints were lost")
	}
	encoded, err := json.Marshal(f.c.Status())
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc([]string{f.cfg.Pools[0].Token, "runner_token_env"}, func(secret string) bool { return strings.Contains(string(encoded), secret) }) {
		t.Fatal("status exposed runner credentials")
	}
}

func TestAdoptionWaitsForVerifiedProviderAndInventory(t *testing.T) {
	for _, failure := range []string{"describe", "identity", "inventory", "missing", "unknown"} {
		t.Run(failure, func(t *testing.T) {
			p := testPool("linux", 1, 1, 0)
			p.AdoptIDs = []string{"owned"}
			f := setup(t, p)
			if f.store.Snapshot().Pools[p.Name].Slots[0].ResourceID != "" || len(f.store.Snapshot().ProviderBindings) != 0 {
				t.Fatal("construction bound an unverified resource or account")
			}
			switch failure {
			case "describe":
				f.host.describeErr = errors.New("unavailable")
			case "identity":
				f.host.description.Identity = "other-account"
			case "inventory":
				f.host.listErr = errors.New("unavailable")
			case "missing":
				delete(f.host.resources, "owned")
			case "unknown":
				f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Unknown}
			}
			if err := f.c.Step(context.Background()); err == nil {
				t.Fatal("unverified adoption was accepted")
			}
			if f.store.Snapshot().Pools[p.Name].Slots[0].ResourceID != "" || f.host.mutations() != 0 {
				t.Fatal("failed verification bound or changed a resource")
			}
		})
	}
}

func TestDisposableDrainAndDestroyRetainCapacityUntilLaterAbsence(t *testing.T) {
	idle := testPool("idle", 1, 1, 0)
	idle.AdoptIDs = []string{"owned"}
	f := setupLifecycle(t, provider.Disposable, idle, testPool("urgent", 1, 1, 10))
	f.cfg.Providers["host"] = testProvider(provider.Disposable, 1)
	f.api.jobs = []state.Job{job(1, "pending", "urgent")}
	f.host.drained = false
	f.step(t, 0)
	if f.host.drains != 1 || f.host.destroys != 0 || f.host.creates != 0 {
		t.Fatal("capacity was released before draining")
	}
	f.host.drained = true
	f.host.onDestroy = func() {
		slot := f.store.Snapshot().Pools["idle"].Slots[0]
		if slot.ResourceID != "owned" || slot.Operation == nil || slot.Operation.Kind != "destroy" || slot.Operation.Phase != "requested" {
			t.Error("destroy lacked a durable resource identity and request")
		}
	}
	f.step(t, 15*time.Second)
	if f.host.destroys != 1 || f.host.creates != 0 || f.store.Snapshot().Pools["idle"].Slots[0].ResourceID != "owned" {
		t.Fatal("destroy acknowledgment freed reserved capacity")
	}
	f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Stopped}
	f.step(t, 15*time.Second)
	if f.host.destroys != 1 || f.host.creates != 0 || f.store.Snapshot().Pools["idle"].Slots[0].ResourceID != "owned" {
		t.Fatal("a stopped but present resource freed disposable capacity")
	}
	delete(f.host.resources, "owned")
	f.step(t, 15*time.Second)
	if f.host.creates != 1 || f.store.Snapshot().Pools["idle"].Slots[0].ResourceID != "" {
		t.Fatal("later complete inventory did not release disposable capacity")
	}
	f.step(t, 15*time.Second)
	f.restart(t)
	f.step(t, 15*time.Second)
	if f.host.creates != 1 || f.store.Snapshot().Pools["idle"].Slots[0].ResourceID != "" {
		t.Fatal("a consumed bootstrap ID was adopted again after destruction and restart")
	}
	f.host.assertDisposableCalls(t)
}

func TestDisposableLostDestroyResponseSurvivesRestartWithoutDrainingAgain(t *testing.T) {
	p := testPool("linux", 1, 1, 0)
	p.AdoptIDs = []string{"owned"}
	f := setupLifecycle(t, provider.Disposable, p)
	f.step(t, 0)
	f.host.destroyErr = &provider.Error{Operation: "destroy", Kind: provider.Uncertain, Detail: "response lost"}
	f.step(t, time.Minute)
	slot := f.store.Snapshot().Pools[p.Name].Slots[0]
	if slot.ResourceID != "owned" || slot.Operation == nil || slot.Operation.Kind != "destroy" || slot.Operation.Phase != "requested" || f.host.drains != 1 {
		t.Fatal("lost response discarded the durable destroy request")
	}
	f.restart(t)
	f.step(t, 15*time.Second)
	if f.host.destroys != 1 || f.host.drains != 1 {
		t.Fatal("restart repeated a release before the observation grace period")
	}
	f.host.destroyErr = nil
	f.step(t, time.Duration(f.cfg.ObservationGrace))
	if f.host.destroys != 2 || f.host.drains != 1 || f.store.Snapshot().Pools[p.Name].Slots[0].ResourceID != "owned" {
		t.Fatal("retry discarded ownership or repeated the completed drain")
	}
	delete(f.host.resources, "owned")
	f.step(t, 15*time.Second)
	slot = f.store.Snapshot().Pools[p.Name].Slots[0]
	if slot.ResourceID != "" || slot.Operation != nil || slot.Power != provider.Stopped {
		t.Fatal("observed absence did not complete the durable destroy request")
	}
	f.host.assertDisposableCalls(t)
}

func TestDisposableBootstrapCanBeRecreatedAcrossRestart(t *testing.T) {
	p := testPool("linux", 1, 1, 0)
	p.AdoptIDs = []string{"bootstrap"}
	f := setupLifecycle(t, provider.Disposable, p)
	f.host.destroyVisible = true
	f.step(t, 0)
	f.step(t, time.Minute)
	if f.host.destroys != 1 || f.store.Snapshot().Pools[p.Name].Slots[0].ResourceID != "bootstrap" {
		t.Fatal("destroy acknowledgment did not preserve the bootstrap reservation")
	}
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, time.Minute)
	id := f.store.Snapshot().Pools[p.Name].Slots[0].ResourceID
	if id == "" || id == "bootstrap" || f.host.creates != 1 {
		t.Fatal("verified absence and fresh demand did not create a new resource")
	}
	f.restart(t)
	f.step(t, 15*time.Second)
	if slot := f.store.Snapshot().Pools[p.Name].Slots[0]; slot.ResourceID != id || !slot.Ready || f.host.creates != 1 {
		t.Fatal("restart replaced the fresh resource with the consumed bootstrap ID")
	}
	f.host.assertDisposableCalls(t)
}

func TestDisposableCreationWaitsForRunningWithoutResume(t *testing.T) {
	for _, starts := range []bool{false, true} {
		name := "stays-stopped"
		if starts {
			name = "starts-later"
		}
		t.Run(name, func(t *testing.T) {
			f := setupLifecycle(t, provider.Disposable, testPool("linux", 1, 1, 0))
			f.host.createState = provider.Stopped
			f.api.jobs = []state.Job{job(1, "pending", "linux")}
			f.step(t, 0)
			id := f.store.Snapshot().Pools["linux"].Slots[0].ResourceID
			f.step(t, 15*time.Second)
			slot := f.store.Snapshot().Pools["linux"].Slots[0]
			if slot.Operation == nil || slot.Operation.Kind != "await-running" || f.host.destroys != 0 {
				t.Fatal("lagging startup was treated as an idle stopped resource")
			}
			if starts {
				f.host.resources[id] = provider.Resource{ID: id, State: provider.Running}
			}
			f.step(t, time.Duration(f.cfg.ObservationGrace))
			if !starts {
				if f.host.destroys != 0 {
					t.Fatal("observation grace ended bootstrap before its readiness deadline")
				}
				f.step(t, time.Duration(f.cfg.ReadinessTimeout))
			}
			slot = f.store.Snapshot().Pools["linux"].Slots[0]
			if starts && (!slot.Ready || slot.Operation != nil || f.host.configures != 1 || f.host.destroys != 0) {
				t.Fatal("later running inventory did not configure the disposable runner")
			}
			if !starts && (f.host.destroys != 1 || f.host.drains != 0 || slot.ResourceID != id || slot.Operation == nil || slot.Operation.Kind != "destroy") {
				t.Fatal("stopped disposable startup was not destroyed with its reservation intact")
			}
			f.host.assertDisposableCalls(t)
		})
	}
}

func TestUnexpectedMissingDisposableResourceKeepsItsReservation(t *testing.T) {
	f := setupLifecycle(t, provider.Disposable, testPool("linux", 1, 1, 0))
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, 0)
	f.step(t, 15*time.Second)
	id := f.store.Snapshot().Pools["linux"].Slots[0].ResourceID
	delete(f.host.resources, id)
	f.step(t, 2*time.Minute)
	f.restart(t)
	f.step(t, time.Minute)
	slot := f.store.Snapshot().Pools["linux"].Slots[0]
	if slot.ResourceID != id || slot.Power != provider.Unknown || f.host.creates != 1 || f.host.destroys != 0 {
		t.Fatal("unexpected absence allowed replacement or lost the resource reservation")
	}
	f.host.assertDisposableCalls(t)
}

func TestCreationRetriesRequireExplicitNoSideEffectEvidence(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		retry bool
	}{
		{"generic", errors.New("response unavailable"), false},
		{"transient", &provider.Error{Operation: "create", Kind: provider.Transient, Detail: "unavailable"}, false},
		{"invalid", &provider.Error{Operation: "create", Kind: provider.Invalid, Detail: "rejected"}, false},
		{"explicit-rejection", &provider.Error{Operation: "create", Kind: provider.Invalid, Detail: "rejected", NoSideEffect: true}, true},
		{"uncertain-overrides-rejection", &provider.Error{Operation: "create", Kind: provider.Uncertain, Detail: "response lost", NoSideEffect: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, testPool("linux", 1, 1, 0))
			f.api.jobs = []state.Job{job(1, "pending", "linux")}
			f.host.createErr = tc.err
			f.step(t, 0)
			op := f.store.Snapshot().Pools["linux"].Slots[0].Operation
			if tc.retry && op != nil || !tc.retry && (op == nil || op.Kind != "unresolved") {
				t.Fatal("creation result did not preserve the required reservation")
			}
			f.restart(t)
			f.host.createErr = nil
			f.step(t, time.Minute)
			want := 1
			if tc.retry {
				want++
			}
			if f.host.creates != want {
				t.Fatalf("created %d times, want %d", f.host.creates, want)
			}
		})
	}
}

func TestProviderBindingChangesCannotReinterpretOwnedHandles(t *testing.T) {
	for _, change := range []string{"account", "scope", "lifecycle", "absence-confirmation"} {
		t.Run(change, func(t *testing.T) {
			p := testPool("linux", 1, 1, 0)
			p.AdoptIDs = []string{"owned"}
			f := setup(t, p)
			f.api.jobs = []state.Job{job(1, "running", "linux")}
			f.step(t, 0)
			previous := f.store.Snapshot().ProviderBindings["host"]
			spec := f.cfg.Providers["host"]
			switch change {
			case "account":
				f.host.description.Identity, spec.Identity = "other-account", "other-account"
			case "scope":
				f.host.description.Scope = "other-scope"
			case "lifecycle":
				f.host.description.Lifecycle, spec.Lifecycle = provider.Disposable, provider.Disposable
				f.host.description.AbsenceConfirmsDestroy = true
			case "absence-confirmation":
				f.host.description.AbsenceConfirmsDestroy = true
			}
			f.cfg.Providers["host"] = spec
			lists, mutations := f.host.lists, f.host.mutations()
			f.now = f.now.Add(time.Minute)
			if err := f.c.Step(context.Background()); err == nil {
				t.Fatal("owned resource was reinterpreted under a different provider binding")
			}
			if f.host.lists != lists || f.host.mutations() != mutations || f.store.Snapshot().ProviderBindings["host"] != previous {
				t.Fatal("binding mismatch allowed inventory, mutations, or a saved binding change")
			}
		})
	}
}

func TestDuplicateProviderAliasesCannotAllocateTheSameAccount(t *testing.T) {
	f := setup(t, testPool("linux", 1, 1, 0))
	alias := newFakeProvider(provider.Retained)
	f.cfg.Providers["alias"] = testProvider(provider.Retained, 5)
	c, err := New(f.cfg, f.store, f.api, map[string]provider.Provider{"host": f.host, "alias": alias}, f.c.log)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return f.now }
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	if err := c.Step(context.Background()); err == nil {
		t.Fatal("duplicate account and scope aliases were accepted")
	}
	if f.host.mutations()+alias.mutations() != 0 || f.store.Snapshot().Pools["linux"].Slots[0].ResourceID != "" {
		t.Fatal("duplicate aliases allocated or adopted a resource")
	}
}

func TestComputeOptionsCannotChangeWhileSavedHandlesExist(t *testing.T) {
	p := testPool("linux", 1, 1, 0)
	p.Options = json.RawMessage(`{"cpu":2,"memory":4}`)
	f := setup(t, p)
	f.api.jobs = []state.Job{job(1, "pending", "linux")}
	f.step(t, 0)
	previous := f.store.Snapshot().Pools[p.Name].OptionsHash
	f.cfg.Pools[0].Options = json.RawMessage(`{"memory":4,"cpu":2}`)
	f.construct(t)
	f.cfg.Pools[0].Options = json.RawMessage(`{"cpu":4,"memory":4}`)
	if _, err := New(f.cfg, f.store, f.api, map[string]provider.Provider{"host": f.host}, f.c.log); err == nil {
		t.Fatal("compute options changed while a saved handle existed")
	}
	if f.store.Snapshot().Pools[p.Name].OptionsHash != previous || f.host.creates != 1 {
		t.Fatal("rejected compute options changed ownership or allocated another resource")
	}
}

func TestRuntimeAndTokenUpdatesWaitForIdleStopAndResume(t *testing.T) {
	p := testPool("linux", 1, 1, 0)
	p.AdoptIDs = []string{"owned"}
	f := setup(t, p)
	f.api.jobs = []state.Job{job(1, "running", "linux")}
	f.step(t, 0)
	previous := f.store.Snapshot().Pools[p.Name].Slots[0].RuntimeRevision
	f.cfg.Runtime = json.RawMessage(`{"executor":"updated"}`)
	f.cfg.Pools[0].Token = "glrt-rotated-test"
	spec := f.cfg.Providers["host"]
	spec.Revision = "2"
	f.cfg.Providers["host"] = spec
	f.construct(t)
	f.step(t, 15*time.Second)
	if f.host.configures != 1 || f.store.Snapshot().Pools[p.Name].Slots[0].RuntimeRevision != previous {
		t.Fatal("healthy running manager was reconfigured during active work")
	}
	f.api.jobs = nil
	f.step(t, time.Minute)
	f.step(t, time.Minute)
	f.step(t, 15*time.Second)
	f.api.jobs = []state.Job{job(2, "pending", "linux")}
	f.step(t, time.Minute)
	f.step(t, 15*time.Second)
	latest := f.host.specs[len(f.host.specs)-1]
	if f.host.stops != 1 || f.host.resumes != 1 || f.host.configures != 2 || latest.Token != f.cfg.Pools[0].Token || string(latest.Options) != string(f.cfg.Runtime) || latest.Revision == previous {
		t.Fatalf("idle release did not apply the current configuration: stops=%d resumes=%d configures=%d runtime=%s", f.host.stops, f.host.resumes, f.host.configures, latest.Options)
	}
}

func (p *fakeProvider) mutations() int {
	return p.creates + p.resumes + p.persists + p.configures + p.drains + p.stops + p.destroys
}

func (p *fakeProvider) assertDisposableCalls(t *testing.T) {
	t.Helper()
	if p.resumes != 0 || p.persists != 0 || p.stops != 0 {
		t.Fatalf("disposable provider received retained operations: resume=%d persist=%d stop=%d", p.resumes, p.persists, p.stops)
	}
}
