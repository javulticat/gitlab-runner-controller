package controller

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"

	"github.com/javulticat/gitlab-runner-controller/internal/state"
	"github.com/javulticat/gitlab-runner-controller/pkg/provider"
)

func TestMetricsReadinessSnapshot(t *testing.T) {
	now := time.Date(2026, time.November, 12, 12, 0, 0, 0, time.UTC)
	saved := state.State{
		LastSync: now.Add(-time.Second), BackoffUntil: now.Add(time.Minute),
		Pools: map[string]state.Pool{
			"workers": {Slots: []state.Slot{
				{ResourceID: "ready", Power: provider.Running, Ready: true, BootstrapStartedAt: now.Add(-time.Hour), BootstrapDeadline: now.Add(-time.Minute)},
				{ResourceID: "starting", Power: provider.Running, BootstrapStartedAt: now.Add(-30 * time.Second), BootstrapDeadline: now.Add(time.Minute)},
				{ResourceID: "expired", Power: provider.Running, BootstrapStartedAt: now.Add(-5 * time.Minute), BootstrapDeadline: now},
				{ResourceID: "unknown", Power: provider.Unknown, BootstrapStartedAt: now.Add(-20 * time.Minute), BootstrapDeadline: now.Add(-time.Minute)},
				{ResourceID: "stopped", Power: provider.Stopped},
				{Power: provider.Unknown, Operation: &state.Operation{Kind: "unresolved"}, BootstrapStartedAt: now.Add(-time.Hour), BootstrapDeadline: now.Add(-time.Minute)},
			}},
			"future": {Slots: []state.Slot{
				{ResourceID: "future", Power: provider.Running, BootstrapStartedAt: now.Add(time.Minute), BootstrapDeadline: now.Add(2 * time.Minute)},
			}},
			"idle": {Slots: []state.Slot{{Power: provider.Stopped}}},
		},
		Jobs: map[string]state.Job{
			"1": {Status: "pending"}, "2": {Status: "pending"}, "3": {Status: "preparing"},
			"4": {Status: "running"}, "5": {Status: "canceling"}, "6": {Status: "success"},
		},
	}
	m := NewMetrics()
	m.ObserveAt(saved, now, time.Second, nil)
	want := map[string]float64{
		"grc_instances;pool=workers;state=running":   3,
		"grc_instances;pool=workers;state=stopped":   1,
		"grc_instances;pool=workers;state=unknown":   2,
		"grc_runner_ready;pool=workers":              1,
		"grc_runner_not_ready;pool=workers":          2,
		"grc_bootstrap_timeouts;pool=workers":        2,
		"grc_bootstrap_pending_seconds;pool=workers": 1200,
		"grc_unresolved_operations;pool=workers":     1,
		"grc_instances;pool=future;state=running":    1,
		"grc_instances;pool=future;state=stopped":    0,
		"grc_instances;pool=future;state=unknown":    0,
		"grc_runner_ready;pool=future":               0,
		"grc_runner_not_ready;pool=future":           1,
		"grc_bootstrap_timeouts;pool=future":         0,
		"grc_bootstrap_pending_seconds;pool=future":  0,
		"grc_unresolved_operations;pool=future":      0,
		"grc_instances;pool=idle;state=running":      0,
		"grc_instances;pool=idle;state=stopped":      1,
		"grc_instances;pool=idle;state=unknown":      0,
		"grc_runner_ready;pool=idle":                 0,
		"grc_runner_not_ready;pool=idle":             0,
		"grc_bootstrap_timeouts;pool=idle":           0,
		"grc_bootstrap_pending_seconds;pool=idle":    0,
		"grc_unresolved_operations;pool=idle":        0,
		"grc_jobs;status=pending":                    2,
		"grc_jobs;status=preparing":                  1,
		"grc_jobs;status=running":                    1,
		"grc_jobs;status=canceling":                  1,
		"grc_gitlab_last_sync_timestamp_seconds":     float64(saved.LastSync.Unix()),
		"grc_gitlab_backoff_until_timestamp_seconds": float64(saved.BackoffUntil.Unix()),
	}
	if got := gatherStateGauges(t, m.registry); !maps.Equal(got, want) {
		t.Fatalf("state metrics = %v, want %v", got, want)
	}
	saved.Pools["workers"].Slots[0].Ready = false
	delete(saved.Pools, "idle")
	delete(saved.Jobs, "1")
	if got := gatherStateGauges(t, m.registry); !maps.Equal(got, want) {
		t.Fatalf("published metrics changed with source state: %v", got)
	}
	m.ObserveAt(state.State{}, now, time.Second, nil)
	want = map[string]float64{
		"grc_jobs;status=pending":                    0,
		"grc_jobs;status=preparing":                  0,
		"grc_jobs;status=running":                    0,
		"grc_jobs;status=canceling":                  0,
		"grc_gitlab_last_sync_timestamp_seconds":     0,
		"grc_gitlab_backoff_until_timestamp_seconds": 0,
	}
	if got := gatherStateGauges(t, m.registry); !maps.Equal(got, want) {
		t.Fatalf("replacement state metrics = %v, want %v", got, want)
	}
}

func TestMetricsConcurrentScrapesSeeCompleteSnapshots(t *testing.T) {
	now := time.Date(2026, time.November, 12, 12, 0, 0, 0, time.UTC)
	snapshots := []state.State{
		{
			Pools: map[string]state.Pool{"workers": {Slots: make([]state.Slot, 100)}},
			Jobs:  map[string]state.Job{"1": {Status: "pending"}}, LastSync: now, BackoffUntil: now.Add(time.Minute),
		},
		{
			Pools: map[string]state.Pool{"workers": {Slots: make([]state.Slot, 100)}},
			Jobs:  map[string]state.Job{"1": {Status: "running"}},
		},
	}
	for i := range snapshots[0].Pools["workers"].Slots {
		id := "resource-" + strconv.Itoa(i)
		snapshots[0].Pools["workers"].Slots[i] = state.Slot{Power: provider.Running, ResourceID: id, Ready: true}
		snapshots[1].Pools["workers"].Slots[i] = state.Slot{
			Power: provider.Running, ResourceID: id, BootstrapStartedAt: now.Add(-time.Minute), BootstrapDeadline: now,
		}
	}
	m := NewMetrics()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(m.state)
	want := make([]map[string]float64, len(snapshots))
	for i, saved := range snapshots {
		m.ObserveAt(saved, now, time.Second, nil)
		want[i] = gatherStateGauges(t, registry)
	}
	started, done := make(chan struct{}), make(chan struct{})
	var workers sync.WaitGroup
	workers.Go(func() {
		for i := 0; ; i++ {
			m.ObserveAt(snapshots[i%len(snapshots)], now, time.Second, nil)
			if i == 0 {
				close(started)
			}
			select {
			case <-done:
				return
			default:
			}
		}
	})
	t.Cleanup(func() {
		close(done)
		workers.Wait()
	})
	<-started
	for range 200 {
		got := gatherStateGauges(t, registry)
		if !maps.Equal(got, want[0]) && !maps.Equal(got, want[1]) {
			t.Fatalf("scrape observed an incomplete or mixed snapshot: %v", got)
		}
	}
}

func TestMetricsKeepStandardCollectorsAndExcludePrivateValues(t *testing.T) {
	const secret = "glrt-private-metrics-token"
	m := NewMetrics()
	saved := state.State{
		Pools: map[string]state.Pool{"workers": {Slots: []state.Slot{{ResourceID: secret, Power: provider.Unknown, LastError: secret}}}},
		Jobs:  map[string]state.Job{secret: {Status: "running", Tags: []string{secret}}},
	}
	m.Observe(saved, time.Second, errors.New(secret))
	m.Observe(saved, 2*time.Second, nil)
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var cycles, duration bool
	seen := make(map[string]bool)
	for _, family := range families {
		seen[family.GetName()] = true
		switch family.GetName() {
		case "grc_reconciliations_total":
			cycles = len(family.Metric) == 2
			for _, metric := range family.Metric {
				if len(metric.Label) != 1 || metric.Label[0].GetName() != "result" || (metric.Label[0].GetValue() != "success" && metric.Label[0].GetValue() != "error") || metric.GetCounter().GetValue() != 1 {
					t.Fatalf("unexpected reconciliation result counter: %v", metric)
				}
			}
		case "grc_reconciliation_duration_seconds":
			duration = len(family.Metric) == 1 && family.Metric[0].GetHistogram().GetSampleCount() == 2 && family.Metric[0].GetHistogram().GetSampleSum() == 3
		}
	}
	if !cycles || !duration || !seen["go_goroutines"] {
		t.Fatalf("standard collectors missing or invalid: cycles=%t duration=%t runtime=%t", cycles, duration, seen["go_goroutines"])
	}
	processRegistry := prometheus.NewRegistry()
	processRegistry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	processFamilies, err := processRegistry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range processFamilies {
		if !seen[family.GetName()] {
			t.Fatalf("process collector missing metric %s", family.GetName())
		}
	}
	controller := Controller{metrics: m}
	recorder := httptest.NewRecorder()
	controller.MetricsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), secret) {
		t.Fatal("metrics exposed a private source value")
	}
}

func gatherStateGauges(t *testing.T, registry prometheus.Gatherer) map[string]float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]float64)
	for _, family := range families {
		if family.GetType() != dto.MetricType_GAUGE || !strings.HasPrefix(family.GetName(), "grc_") {
			continue
		}
		for _, metric := range family.Metric {
			key := family.GetName()
			for _, label := range metric.Label {
				key += ";" + label.GetName() + "=" + label.GetValue()
			}
			if _, exists := values[key]; exists {
				t.Fatalf("duplicate gauge series %s", key)
			}
			values[key] = metric.GetGauge().GetValue()
		}
	}
	return values
}
