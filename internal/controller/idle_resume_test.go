package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func TestIdleRetainedResumeReleasesReservationAcrossRestart(t *testing.T) {
	f, _ := stalledRetainedFixture(t)
	f.step(t, 0)
	initial := f.store.Snapshot().Pools["protected"].Slots[0]
	f.now = initial.BootstrapDeadline
	f.step(t, 0)
	if slot := f.store.Snapshot().Pools["protected"].Slots[0]; slot.LastError == "" || !slot.RetryAt.After(f.now) {
		t.Fatal("test did not enter timed-out resume backoff")
	}
	f.api.jobs = nil
	f.step(t, time.Duration(f.cfg.JobsPollInterval))
	assertCanceledResume(t, f, initial)
	calls := f.host.resumes
	f.restart(t)
	for range 60 {
		f.step(t, time.Minute)
	}
	assertCanceledResume(t, f, initial)
	if f.host.resumes != calls || f.host.creates != 0 || f.host.stops != 0 || f.host.destroys != 0 {
		t.Fatal("idle resume continued remote work after cancellation")
	}
}

func assertCanceledResume(t *testing.T, f *fixture, original state.Slot) {
	t.Helper()
	saved := f.store.Snapshot()
	slot := saved.Pools["protected"].Slots[0]
	if slot.Operation != nil || slot.ResourceID != original.ResourceID || slot.Persisted != original.Persisted || slot.Power != provider.Stopped ||
		slot.Ready || slot.LastError != "" || !slot.RetryAt.IsZero() || !slot.BootstrapStartedAt.IsZero() || !slot.BootstrapDeadline.IsZero() {
		t.Fatalf("idle cancellation did not preserve the stopped retained host: %+v", slot)
	}
	if capacity(saved, map[string]map[string]provider.Resource{"host": f.host.resources}, "host") != 0 || !f.c.Status().Ready {
		t.Fatal("canceled resume still reserved capacity or reported a bootstrap failure")
	}
}

func TestIdleResumeFreesSharedAccountCapacityInTheSameCycle(t *testing.T) {
	f, _ := stalledRetainedFixture(t)
	f.cfg.Providers["host"] = testProvider(provider.Retained, 1)
	f.cfg.Pools = append(f.cfg.Pools, testPool("available", 1, 1, 0))
	f.construct(t)
	f.step(t, 0)
	if f.host.resumes != 1 || f.host.creates != 0 {
		t.Fatal("test did not reserve the shared account for a resume")
	}
	f.api.jobs = []state.Job{job(2, "pending", "available")}
	if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 2, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	f.step(t, 5*time.Second)
	if f.store.Snapshot().Pools["protected"].Slots[0].Operation == nil || f.host.creates != 0 {
		t.Fatal("recent acknowledged resume freed shared quota during boot")
	}
	f.step(t, time.Duration(f.cfg.ObservationGrace)-5*time.Second-time.Second)
	if f.store.Snapshot().Pools["protected"].Slots[0].Operation == nil || f.host.creates != 0 {
		t.Fatal("resume reservation ended before the observation grace")
	}
	f.step(t, time.Second)
	if f.store.Snapshot().Pools["protected"].Slots[0].Operation != nil || f.host.creates != 1 || f.host.resumes != 1 {
		t.Fatal("stopped idle resume prevented another pool from using the account")
	}
}

func TestIdleResumeGraceStartsAfterLatestRetryEvenPastReadinessDeadline(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, uncertain := range []bool{false, true} {
			name := "ack"
			if uncertain {
				name = "uncertain-reply"
			}
			if expired {
				name += "-past-deadline"
			}
			t.Run(name, func(t *testing.T) {
				f, host := stalledRetainedFixture(t)
				f.cfg.Providers["host"] = testProvider(provider.Retained, 1)
				f.cfg.Pools = append(f.cfg.Pools, testPool("available", 1, 1, 0))
				f.construct(t)
				f.step(t, 0)
				initial := f.store.Snapshot().Pools["protected"].Slots[0]
				if expired {
					f.now = initial.BootstrapDeadline
					f.step(t, 0)
					f.now = f.store.Snapshot().Pools["protected"].Slots[0].RetryAt
				} else {
					f.now = initial.ChangedAt.Add(time.Duration(f.cfg.ObservationGrace))
				}
				if uncertain {
					host.resumeErr = &provider.Error{Operation: "resume", Kind: provider.Transient, Detail: "reply was lost"}
				}
				f.step(t, 0)
				latest := f.store.Snapshot().Pools["protected"].Slots[0].ChangedAt
				if !latest.Equal(f.now) || f.host.resumes != 2 {
					t.Fatal("latest resume attempt was not recorded")
				}
				f.api.jobs = []state.Job{job(2, "pending", "available")}
				if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 2, Status: "pending"}); err != nil {
					t.Fatal(err)
				}
				f.step(t, 5*time.Second)
				if f.store.Snapshot().Pools["protected"].Slots[0].Operation == nil || f.host.creates != 0 {
					t.Fatal("recent retry released quota before observation grace")
				}
				f.restart(t)
				f.step(t, time.Duration(f.cfg.ObservationGrace)-5*time.Second-time.Second)
				if slot := f.store.Snapshot().Pools["protected"].Slots[0]; slot.Operation == nil || !slot.ChangedAt.Equal(latest) || f.host.creates != 0 {
					t.Fatal("restart forgot the latest retry's reservation")
				}
				f.step(t, time.Second)
				if f.store.Snapshot().Pools["protected"].Slots[0].Operation != nil || f.host.creates != 1 || f.host.resumes != 2 {
					t.Fatal("expired observation grace did not release idle quota")
				}
			})
		}
	}
}

type timedResumeProvider struct {
	*stalledResumeProvider
	onResume func()
}

func (p *timedResumeProvider) Resume(ctx context.Context, id string) error {
	p.onResume()
	return p.stalledResumeProvider.Resume(ctx, id)
}

func TestIdleResumeGraceUsesCompletionTimeAndDurableDispatchTime(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "ack"
		if uncertain {
			name = "uncertain-reply"
		}
		t.Run(name, func(t *testing.T) {
			f, host := stalledRetainedFixture(t)
			if uncertain {
				host.resumeErr = &provider.Error{Operation: "resume", Kind: provider.Transient, Detail: "reply was lost"}
			}
			f.providers["host"] = &timedResumeProvider{stalledResumeProvider: host, onResume: func() {
				if slot := f.store.Snapshot().Pools["protected"].Slots[0]; !slot.ChangedAt.Equal(f.now) || slot.Operation == nil {
					t.Error("resume was dispatched before its attempt timestamp was durable")
				}
				f.now = f.now.Add(15 * time.Second)
			}}
			f.step(t, 0)
			completed := f.now
			if !f.store.Snapshot().Pools["protected"].Slots[0].ChangedAt.Equal(completed) {
				t.Fatal("resume completion did not extend the observation window")
			}
			f.api.jobs = nil
			if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 1, Status: "success"}); err != nil {
				t.Fatal(err)
			}
			f.step(t, 5*time.Second)
			if f.store.Snapshot().Pools["protected"].Slots[0].Operation == nil {
				t.Fatal("long-running resume was canceled five seconds after its reply")
			}
			f.step(t, time.Duration(f.cfg.ObservationGrace)-5*time.Second)
			if f.store.Snapshot().Pools["protected"].Slots[0].Operation != nil || f.host.resumes != 1 {
				t.Fatal("idle completion window did not end without another remote attempt")
			}
		})
	}
}

func TestCanceledResumeLateStartUsesNormalIdleRelease(t *testing.T) {
	f, _ := stalledRetainedFixture(t)
	f.step(t, 0)
	f.api.jobs = nil
	f.step(t, time.Duration(f.cfg.ObservationGrace))
	if f.store.Snapshot().Pools["protected"].Slots[0].Operation != nil {
		t.Fatal("idle resume was not canceled")
	}
	f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Running}
	f.step(t, 15*time.Second)
	if slot := f.store.Snapshot().Pools["protected"].Slots[0]; !slot.Ready || f.host.configures != 1 {
		t.Fatal("late running host was forgotten or left unmanaged")
	}
	f.step(t, time.Duration(f.cfg.IdleGrace))
	f.step(t, 15*time.Second)
	if slot := f.store.Snapshot().Pools["protected"].Slots[0]; slot.ResourceID != "owned" || slot.Power != provider.Stopped || slot.Operation != nil ||
		f.host.stops != 1 || f.host.resumes != 1 || f.host.creates != 0 || f.host.destroys != 0 {
		t.Fatal("late resume did not follow retained idle stop and observation")
	}
}

type resumeInventoryHint struct {
	*fakeProvider
	onList func()
}

func (p *resumeInventoryHint) List(ctx context.Context) ([]provider.Resource, error) {
	p.onList()
	return p.fakeProvider.List(ctx)
}

func TestIdleResumeCancellationRequiresVerifiedDemandAndStoppedInventory(t *testing.T) {
	for _, condition := range []string{"pending", "busy", "unknown-assignment", "missing-inventory", "unknown-power", "unverified-hint"} {
		t.Run(condition, func(t *testing.T) {
			f, _ := stalledRetainedFixture(t)
			f.step(t, 0)
			f.api.jobs = nil
			switch condition {
			case "pending":
				f.api.jobs = []state.Job{job(1, "pending", "protected")}
			case "busy":
				f.api.jobs = []state.Job{job(1, "running", "protected")}
			case "unknown-assignment":
				busy := job(1, "running", "protected")
				busy.RunnerID = 0
				f.api.jobs = []state.Job{busy}
			case "missing-inventory":
				f.host.listErr = errors.New("inventory unavailable")
			case "unknown-power":
				f.host.resources["owned"] = provider.Resource{ID: "owned", State: provider.Unknown}
			case "unverified-hint":
				f.providers["host"] = &resumeInventoryHint{fakeProvider: f.host, onList: func() {
					if err := f.c.Hint(state.Hint{ProjectID: 1, ID: 99, Status: "pending"}); err != nil {
						t.Error(err)
					}
				}}
			}
			f.now = f.now.Add(max(time.Duration(f.cfg.JobsPollInterval), time.Duration(f.cfg.ObservationGrace)))
			if err := f.c.Step(context.Background()); errors.Is(err, state.ErrPoisoned) {
				t.Fatal(err)
			}
			if slot := f.store.Snapshot().Pools["protected"].Slots[0]; slot.Operation == nil || slot.Operation.Kind != "resume" || slot.ResourceID != "owned" {
				t.Fatal("uncertain observation or remaining demand canceled a resume")
			}
		})
	}
}
