package gym

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jamespham8929/platform-mcp/internal/agent"
	"github.com/jamespham8929/platform-mcp/internal/eval"
)

// binaries finds incident-cli, prometheus and promtool, or skips the test.
// INCIDENT_CLI must be set, as for the other contract tests. PROMETHEUS and
// PROMTOOL default to the ones on PATH.
func binaries(t *testing.T) (incident, prom, promtool string) {
	t.Helper()
	incident = os.Getenv("INCIDENT_CLI")
	if incident == "" {
		t.Skip("set INCIDENT_CLI to run replays against the real incident-cli")
	}
	find := func(env, name string) string {
		if p := os.Getenv(env); p != "" {
			return p
		}
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s is not on PATH and %s is not set", name, env)
		}
		return p
	}
	return incident, find("PROMETHEUS", "prometheus"), find("PROMTOOL", "promtool")
}

// syntheticRecording writes a small recording. checkout's calls to inventory
// take 5ms for 90 minutes, then 400ms for the last 6. An unrelated service
// deployed 5 minutes before the page.
func syntheticRecording(t *testing.T) Loaded {
	t.Helper()
	dir := t.TempDir()
	detected := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	start := detected.Add(-96 * time.Minute)
	shift := detected.Add(-6 * time.Minute)

	les := []string{"0.005", "0.5", "+Inf"}
	buckets := map[string]*Series{}
	for _, le := range les {
		buckets[le] = &Series{Labels: map[string]string{
			"__name__": "upstream_request_duration_seconds_bucket", "namespace": "gym-synthetic",
			"service": "checkout", "upstream": "inventory", "le": le,
		}}
	}
	counts := map[string]int{}
	for ts := start; !ts.After(detected.Add(time.Minute)); ts = ts.Add(15 * time.Second) {
		for _, le := range les {
			if le != "0.005" || ts.Before(shift) {
				counts[le] += 100
			}
			b := buckets[le]
			b.Samples = append(b.Samples, Sample{
				Time:  json.Number(strconv.FormatInt(ts.Unix(), 10)),
				Value: strconv.Itoa(counts[le]),
			})
		}
	}
	var series []Series
	for _, le := range les {
		series = append(series, *buckets[le])
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	n, err := WriteOpenMetrics(zw, series)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, FileSeries), gz.Bytes(), 0o644))

	deploy := Event{ID: "deploy-recommendations-abc123", Source: "deploy", Service: "recommendations",
		Title: "recommendations v2.4.0: new ranking weights", Timestamp: detected.Add(-5 * time.Minute), Magnitude: 0.6}
	require.NoError(t, writeJSON(filepath.Join(dir, FileEvents), []Event{deploy}))
	require.NoError(t, os.WriteFile(filepath.Join(dir, FileChaos), []byte(`{"items":[]}`), 0o644))

	rec := Recording{
		Format: RecordingFormat,
		Definition: Definition{Name: "synthetic", PagedService: "frontend",
			Fault: &Fault{Type: FaultNetworkLatency, Service: "inventory"},
			Truth: TruthDef{Service: "inventory", Accept: []Rule{{Source: "metric_anomaly", Service: "inventory"}}}},
		Namespace: "gym-synthetic", DetectedAt: detected, Alert: "GymPageLatency",
		SeriesFrom: start, SeriesTo: detected.Add(time.Minute), Series: len(series), Samples: n,
		Truth: Truth{
			Service:     "inventory",
			Accept:      []Rule{{Source: "metric_anomaly", Service: "inventory"}},
			Distractors: []string{"event:" + deploy.ID},
		},
	}
	require.NoError(t, writeJSON(filepath.Join(dir, FileScenario), rec))
	return Loaded{Recording: rec, Dir: dir}
}

func TestReplayFindsTheUpstreamAnomaly(t *testing.T) {
	incident, prom, promtool := binaries(t)
	rec := syntheticRecording(t)

	r := &Replayer{
		IncidentCLI: incident, Prometheus: prom, Promtool: promtool,
		Graph:                "../../gym/graph.json",
		AnomalyQueries:       "../../gym/anomaly-queries.json",
		DefaultWindowMinutes: DefaultWindowMinutes,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	graded, err := r.Run(ctx, []Loaded{rec}, agent.Baseline{})
	require.NoError(t, err)
	require.Len(t, graded, 1)
	g := graded[0]
	require.Empty(t, g.Error)
	require.NotNil(t, g.Result)

	// The anomaly came from the real queries in gym/anomaly-queries.json,
	// run by a real Prometheus over the backfilled samples.
	var anomaly string
	for id, ev := range g.Result.Evidence {
		if ev.Source == "metric_anomaly" {
			anomaly = id
			assert.Equal(t, "inventory", ev.Service, "upstream latency is reported under the upstream")
			assert.Equal(t, "upstream_p99_latency", ev.Query)
		}
	}
	require.NotEmpty(t, anomaly, "no anomaly found; evidence: %v", g.Result.Evidence)
	assert.Contains(t, g.Result.Evidence, "event:deploy-recommendations-abc123")
	assert.True(t, g.Rank == 1 || g.Rank == 2, "rank %d", g.Rank)
	assert.Equal(t, g.Rank == 2, g.Distracted, "the deploy is the only other candidate")
	assert.True(t, g.Upstream)
	assert.Equal(t, 1.0, g.Citations.Share())

	// The window default reached the tool.
	require.Len(t, g.Result.Steps, 1)
	var out struct {
		WindowMinutes int `json:"window_minutes"`
	}
	require.NoError(t, json.Unmarshal(g.Result.Steps[0].Output, &out))
	assert.Equal(t, DefaultWindowMinutes, out.WindowMinutes)
}

func TestGradeMatchesByWhatTheToolSaid(t *testing.T) {
	rec := Recording{Truth: Truth{
		Service:     "inventory",
		Accept:      []Rule{{Source: "metric_anomaly", Service: "inventory"}, {Change: "bad", EvidenceID: "event:deploy-checkout-1"}},
		Distractors: []string{"event:deploy-frontend-2"},
		ChaosRuns:   []string{"gym-x/fault/20261125T120000Z"},
	}}
	res := &agent.Result{
		Answer: &agent.Answer{Status: agent.StatusIdentified, Causes: []agent.Claim{
			{EvidenceID: "event:deploy-frontend-2"},
			{EvidenceID: "event:anomaly:upstream_p99_latency:inventory:1795600000"},
		}},
		Seen: map[string]bool{"event:deploy-frontend-2": true, "event:anomaly:upstream_p99_latency:inventory:1795600000": true},
		Evidence: map[string]agent.Evidence{
			"event:deploy-frontend-2":                                 {Source: "deploy", Service: "frontend"},
			"event:anomaly:upstream_p99_latency:inventory:1795600000": {Source: "metric_anomaly", Service: "inventory", Query: "upstream_p99_latency"},
		},
	}
	var g eval.Graded
	Grade(&g, rec, res)
	assert.Equal(t, 2, g.Rank)
	assert.True(t, g.Distracted)
	assert.False(t, g.Abstained)

	// A symptom on the paged service is not the cause.
	assert.False(t, rec.Truth.Matches("event:anomaly:p99_latency:frontend:1", agent.Evidence{Source: "metric_anomaly", Service: "frontend"}))
	// A made-up ID has no attributes, so it cannot match a rule.
	assert.False(t, rec.Truth.Matches("event:anomaly:made-up", agent.Evidence{}))
	// The exact change and a chaos run both count.
	assert.True(t, rec.Truth.Matches("event:deploy-checkout-1", agent.Evidence{}))
	assert.True(t, rec.Truth.Matches("chaos:gym-x/fault/20261125T120000Z", agent.Evidence{}))
	assert.True(t, rec.Truth.Matches("event:gym-x/fault/20261125T120000Z", agent.Evidence{Source: "chaos", Service: "inventory"}))
	// An unresolved change rule matches nothing.
	assert.False(t, Rule{Change: "bad"}.matches("event:deploy-checkout-1", agent.Evidence{}))
	assert.False(t, Rule{}.matches("event:x", agent.Evidence{Source: "deploy"}))

	var none eval.Graded
	Grade(&none, rec, &agent.Result{})
	assert.True(t, none.Abstained)
}

func TestPrintScenarios(t *testing.T) {
	recs := []Loaded{
		{Recording: Recording{Definition: Definition{Name: "a", Fault: &Fault{Type: "pod-failure", Service: "checkout"}}, Truth: Truth{Service: "checkout"}}},
		{Recording: Recording{Definition: Definition{Name: "b",
			Changes: []Change{{Name: "timeout", Kind: KindConfigChange, Service: "checkout"}},
			Truth:   TruthDef{Service: "checkout", Accept: []Rule{{Change: "timeout"}}},
		}, Truth: Truth{Service: "checkout"}}},
	}
	graded := []eval.Graded{
		{Scenario: "a", Rank: 0, Distracted: true, Result: &agent.Result{
			Answer:   &agent.Answer{Causes: []agent.Claim{{EvidenceID: "event:d"}}},
			Evidence: map[string]agent.Evidence{"event:d": {Source: "deploy", Service: "frontend"}},
		}},
		{Scenario: "b", Rank: 1, Error: "boom"},
	}
	var buf bytes.Buffer
	PrintScenarios(&buf, recs, graded)
	out := buf.String()
	assert.Contains(t, out, "pod-failure on checkout")
	assert.Contains(t, out, "event:d (deploy, frontend)")
	assert.Contains(t, out, "fell for the distraction")
	assert.Contains(t, out, "config change on checkout")
	assert.Contains(t, out, "error: boom")

	buf.Reset()
	PrintScore(&buf, eval.Summarize("baseline", "", graded))
	assert.Contains(t, buf.String(), "distracted")
}

func TestScoreByGroup(t *testing.T) {
	recs := []Loaded{
		{Recording: Recording{Definition: Definition{Name: "a"}}},
		{Recording: Recording{Definition: Definition{Name: "b", Group: GroupHeldOut}}},
		{Recording: Recording{Definition: Definition{Name: "c", Group: GroupHeldOut}}},
	}
	graded := []eval.Graded{{Scenario: "a", Rank: 1}, {Scenario: "b", Rank: 1}, {Scenario: "c", Rank: 0}}
	scores := ScoreByGroup("baseline", "", recs, graded)
	require.Len(t, scores, 2)
	assert.Equal(t, "baseline, tuning", scores[0].Setup)
	assert.Equal(t, 1, scores[0].Scenarios)
	assert.Equal(t, 1.0, scores[0].Top1)
	assert.Equal(t, "baseline, held-out", scores[1].Setup)
	assert.Equal(t, 0.5, scores[1].Top1)
	assert.Len(t, ScoreByGroup("baseline", "", recs[:1], graded[:1]), 1)
}
