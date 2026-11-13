package telemetry_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.yaml.in/yaml/v3"

	"github.com/DrewGGM/aluna/internal/telemetry"
)

// fixed is a Source that reads s, or fails with err.
type fixed struct {
	s   telemetry.State
	err error
}

func (f fixed) Read(context.Context) (telemetry.State, error) { return f.s, f.err }

var pool = telemetry.PoolState{Acquired: 5, Idle: 15, Max: 20}

// collect registers the operations metrics over source at now and collects them once, by name; the collection
// itself must not fail.
func collect(t *testing.T, source telemetry.Source, now time.Time) map[string]metricdata.Metrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	unregister, err := telemetry.Register(provider.Meter("test"), source, func() telemetry.PoolState { return pool }, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unregister(); err != nil {
			t.Error(err)
		}
	})
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	byName := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}
	return byName
}

// value is the value of m's data point with the attribute key=val, or its only one if key is empty.
func value(t *testing.T, m metricdata.Metrics, key, val string) float64 {
	t.Helper()
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		return float64(pointOf(t, data.DataPoints, key, val).Value)
	case metricdata.Gauge[int64]:
		return float64(pointOf(t, data.DataPoints, key, val).Value)
	case metricdata.Gauge[float64]:
		return pointOf(t, data.DataPoints, key, val).Value
	}
	t.Fatalf("metric %s of kind %T", m.Name, m.Data)
	return 0
}

func pointOf[N int64 | float64](t *testing.T, points []metricdata.DataPoint[N], key, val string) metricdata.DataPoint[N] {
	t.Helper()
	for _, p := range points {
		if got, ok := p.Attributes.Value(attribute.Key(key)); key == "" || (ok && got.AsString() == val) {
			return p
		}
	}
	t.Fatalf("no data point with %s=%s", key, val)
	return metricdata.DataPoint[N]{}
}

// points is how many data points m has, of whatever kind.
func points(m metricdata.Metrics) int {
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		return len(data.DataPoints)
	case metricdata.Gauge[int64]:
		return len(data.DataPoints)
	case metricdata.Gauge[float64]:
		return len(data.DataPoints)
	}
	return 0
}

type expected struct {
	name, key, val string
	want           float64
}

func expect(t *testing.T, metrics map[string]metricdata.Metrics, cases []expected) {
	t.Helper()
	for _, c := range cases {
		m, ok := metrics[c.name]
		if !ok {
			t.Errorf("no metric %s", c.name)
			continue
		}
		if got := value(t, m, c.key, c.val); got != c.want {
			t.Errorf("%s{%s=%s} = %v, want %v", c.name, c.key, c.val, got, c.want)
		}
	}
}

// TestTheOperationsMetricsReportTheState (FF-6): each metric reports what the source read, the pool's saturation
// what the pool says, and the unsealed audit age counts from the oldest record waiting.
func TestTheOperationsMetricsReportTheState(t *testing.T) {
	now := time.Date(2026, time.March, 2, 10, 0, 0, 0, time.UTC)
	metrics := collect(t, fixed{s: telemetry.State{
		Postings: 1200, PendingHolds: 7, OpenBreaks: 2, ApprovedRequests: 31, OldestUnsealed: now.Add(-90 * time.Second),
		QueueDepth: map[string]int64{"default": 0, "webhooks": 9},
	}}, now)

	expect(t, metrics, []expected{
		{"aluna.ledger.postings", "", "", 1200},
		{"aluna.ledger.holds.pending", "", "", 7},
		{"aluna.recon.breaks.open", "", "", 2},
		{"aluna.approvals.approved", "", "", 31},
		{"aluna.audit.unsealed.age", "", "", 90},
		{"aluna.jobs.queue.depth", "queue", "webhooks", 9},
		{"aluna.jobs.queue.depth", "queue", "default", 0},
		{"aluna.db.pool.connections", "state", "acquired", 5},
		{"aluna.db.pool.connections", "state", "idle", 15},
		{"aluna.db.pool.max", "", "", 20},
	})
}

// TestNothingWaitingForItsSealIsAgeZero: with every record sealed the age is 0, so a quiet night does not page.
func TestNothingWaitingForItsSealIsAgeZero(t *testing.T) {
	metrics := collect(t, fixed{s: telemetry.State{QueueDepth: map[string]int64{}}}, time.Now())
	expect(t, metrics, []expected{{"aluna.audit.unsealed.age", "", "", 0}})
}

// TestASourceThatFailsLeavesTheRest: the collection does not fail, so the process still exports the pool's metrics
// and those of the requests, and only the database's are missing.
func TestASourceThatFailsLeavesTheRest(t *testing.T) {
	metrics := collect(t, fixed{err: errors.New("the database is down")}, time.Now())

	expect(t, metrics, []expected{{"aluna.db.pool.connections", "state", "acquired", 5}, {"aluna.db.pool.max", "", "", 20}})
	for name, m := range metrics {
		if !strings.HasPrefix(name, "aluna.db.pool") && points(m) > 0 {
			t.Errorf("%s reported without a source", name)
		}
	}
}

// prometheusName is m's name as Prometheus gets it over OTLP: dots become underscores, the unit s adds _seconds and a
// counter adds _total.
func prometheusName(m metricdata.Metrics) string {
	name := strings.ReplaceAll(m.Name, ".", "_")
	if m.Unit == "s" {
		name += "_seconds"
	}
	if _, counter := m.Data.(metricdata.Sum[int64]); counter {
		name += "_total"
	}
	return name
}

// TestTheAlertsUseTheMetrics: every aluna_ metric the alerts and the SLOs query is one the process exports, and both
// files are valid YAML.
func TestTheAlertsUseTheMetrics(t *testing.T) {
	metrics := collect(t, fixed{s: telemetry.State{QueueDepth: map[string]int64{"default": 1}}}, time.Now())
	exported := []string{}
	for _, m := range metrics {
		exported = append(exported, prometheusName(m))
	}
	for _, file := range []string{"alerts.yaml", "slo.yaml"} {
		raw, err := os.ReadFile("../../deploy/observability/" + file) //nolint:gosec // the repository's own rules
		if err != nil {
			t.Fatal(err)
		}
		var parsed any
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, used := range regexp.MustCompile(`\baluna_[a-z_]+`).FindAllString(string(raw), -1) {
			if !slices.Contains(exported, used) {
				t.Errorf("%s queries %s, which is not exported (%v)", file, used, exported)
			}
		}
	}
}
