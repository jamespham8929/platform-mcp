package gym

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCluster answers the kubectl and helm calls the recorder makes, and plays
// Prometheus. The page fires a while after the fault goes in, like the real
// rule with its three minute wait.
type fakeCluster struct {
	mu      sync.Mutex
	now     time.Time
	created time.Time
	calls   []string
	applied map[string]any
	faultAt time.Time
	// triggerAt is when the cause went in: the fault, or for a control the
	// first deploy. The page fires pageAfter later.
	triggerAt time.Time
	pageAfter time.Duration
	// runsAfter is how long after the fault chaos-operator writes the run
	// record. A latency run is written when it ends.
	runsAfter time.Duration
	// outcome is what the first run says.
	outcome  string
	injected bool
	query    string
}

func (f *fakeCluster) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeCluster) Sleep(_ context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return nil
}

func (f *fakeCluster) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	switch {
	case strings.Contains(line, "get namespace"):
		return []byte(f.created.Format(time.RFC3339)), nil
	case strings.Contains(line, "apply -f -"):
		if err := json.Unmarshal(stdin, &f.applied); err != nil {
			return nil, err
		}
		f.faultAt = f.now
		if f.triggerAt.IsZero() {
			f.triggerAt = f.now
		}
		return nil, nil
	case strings.HasPrefix(line, "helm upgrade"):
		if f.triggerAt.IsZero() {
			f.triggerAt = f.now
		}
		return nil, nil
	case strings.Contains(line, "get chaosexperiments"):
		if f.applied == nil || f.now.Before(f.faultAt.Add(f.runsAfter)) {
			return []byte(`{"items":[]}`), nil
		}
		// A scheduled fault starts at the next minute, so its first run is a
		// little after the experiment was created.
		start := f.faultAt.Add(20 * time.Second).UTC()
		return fmt.Appendf(nil, `{"items":[{"metadata":{"name":"fault","namespace":"gym-x"},
			"status":{"runs":[
			{"id":"gym-x/fault/%s","startedAt":%q,"finishedAt":%q,"type":"pod-failure","service":"checkout",
			 "trigger":"schedule","outcome":%q,"injected":%t,"podsAffected":2},
			{"id":"gym-x/fault/late","startedAt":%q,"finishedAt":%q,"type":"pod-failure","service":"checkout",
			 "trigger":"schedule","outcome":"completed","injected":true,"podsAffected":2}]}}]}`,
			start.Format("20060102T150405Z"), start.Format(time.RFC3339), start.Add(time.Second).Format(time.RFC3339),
			f.outcome, f.injected, f.now.Add(time.Hour).UTC().Format(time.RFC3339), f.now.Add(time.Hour).UTC().Format(time.RFC3339)), nil
	}
	return nil, nil
}

func (f *fakeCluster) prometheus(t *testing.T, ns string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.URL.Path {
		case "/api/v1/alerts":
			// The real API answers 405 to a POST here.
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"method not allowed"}`)
				return
			}
			f.mu.Lock()
			firing := !f.triggerAt.IsZero() && f.now.After(f.triggerAt.Add(f.pageAfter))
			f.mu.Unlock()
			state := "pending"
			if firing {
				state = "firing"
			}
			fmt.Fprintf(w, `{"status":"success","data":{"alerts":[
				{"labels":{"alertname":"GymPageErrors","gym_namespace":%q},"state":%q,"activeAt":"2026-11-25T12:00:00Z"},
				{"labels":{"alertname":"GymPageLatency","gym_namespace":"gym-other"},"state":"firing","activeAt":"2026-11-25T12:00:00Z"}]}}`,
				ns, state)
		case "/api/v1/query":
			f.mu.Lock()
			f.query = r.Form.Get("query")
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"__name__":"upstream_requests_total","namespace":"gym-x","service":"frontend","upstream":"checkout","code":"502"},
				 "values":[[1795600000.123,"1"],[1795600015.123,"7"]]},
				{"metric":{"__name__":"http_requests_total","namespace":"gym-x","service":"frontend","code":"201","route":"/api/v1/orders"},
				 "values":[[1795600000,"10"]]}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func newFake(start time.Time) *fakeCluster {
	return &fakeCluster{now: start, created: start.Add(-10 * time.Minute), pageAfter: 4 * time.Minute,
		outcome: "completed", injected: true}
}

func newRecorder(f *fakeCluster, url, out string) *Recorder {
	n := 0
	return &Recorder{
		Kube: f, Prom: Prom{BaseURL: url}, KubeContext: "kind-gym", Chart: "/charts/sample-service",
		Out: out, Warmup: 95 * time.Minute, Settle: time.Minute, PageTimeout: 10 * time.Minute,
		Now: f.Now, Sleep: f.Sleep,
		ID: func() string { n++; return fmt.Sprintf("00000%d", n) },
	}
}

func TestRecordAChaosScenario(t *testing.T) {
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	f := newFake(start)
	d := Definition{
		Name: "x", PagedService: "frontend",
		Fault: &Fault{Type: FaultPodFailure, Service: "checkout", Schedule: "* * * * *",
			Spec: map[string]any{"podFailure": map[string]any{"percentage": 100}}},
		Changes: []Change{
			{Name: "flag", AtSeconds: -120, Kind: KindFeatureFlag, Service: "frontend", Flag: "new-button", Title: "frontend: flag new-button turned on"},
			{Name: "late-deploy", AtSeconds: 60, Kind: KindDeploy, Service: "checkout", Title: "checkout v2", Set: map[string]string{"b": "2", "a": "1"}},
		},
		Truth: TruthDef{Service: "checkout", Accept: []Rule{{Source: "metric_anomaly", Service: "checkout"}}},
	}
	require.NoError(t, d.Validate())
	srv := f.prometheus(t, "gym-x")
	defer srv.Close()
	out := t.TempDir()

	rec, err := newRecorder(f, srv.URL, out).Record(context.Background(), d)
	require.NoError(t, err)

	// It waited for the warmup, measured from the namespace's creation.
	zero := f.created.Add(95*time.Minute + 120*time.Second + 5*time.Second)
	assert.Equal(t, zero, f.faultAt, "the fault goes in once the warmup and the earliest change are done")

	// The calls happen in time order: flag, fault, deploy.
	var order []string
	for _, c := range f.calls {
		switch {
		case strings.Contains(c, "patch configmap flags"):
			order = append(order, "flag")
			assert.Contains(t, c, `{"data":{"new-button":"on"}}`)
		case strings.Contains(c, "apply -f -"):
			order = append(order, "fault")
		case strings.HasPrefix(c, "helm upgrade"):
			order = append(order, "deploy")
			assert.Contains(t, c, "helm upgrade checkout /charts/sample-service --kube-context kind-gym --namespace gym-x --reuse-values")
			assert.Contains(t, c, "--set podAnnotations.gym-release=000002 --set a=1 --set b=2", "extra values come in a fixed order")
		}
	}
	assert.Equal(t, []string{"flag", "fault", "deploy"}, order)

	spec := f.applied["spec"].(map[string]any)
	assert.Equal(t, "pod-failure", spec["type"])
	assert.Equal(t, "* * * * *", spec["schedule"])
	assert.NotContains(t, spec, "container", "pod-failure needs no sidecar")
	assert.Equal(t, map[string]any{"percentage": float64(100)}, spec["podFailure"])
	assert.Equal(t, "fault", f.applied["metadata"].(map[string]any)["name"])

	// The page time and the truth.
	assert.Equal(t, "GymPageErrors", rec.Alert)
	assert.True(t, rec.DetectedAt.After(f.faultAt.Add(4*time.Minute)) && rec.DetectedAt.Before(f.faultAt.Add(4*time.Minute+6*time.Second)),
		"detected %s, fault %s", rec.DetectedAt, f.faultAt)
	require.NotNil(t, rec.FaultAt)
	assert.Equal(t, f.faultAt.Add(20*time.Second).UTC(), *rec.FaultAt, "fault_at comes from the first run record")
	assert.Equal(t, []string{"gym-x/fault/" + f.faultAt.Add(20*time.Second).UTC().Format("20060102T150405Z")},
		rec.Truth.ChaosRuns, "a run that started after the page is not part of the cause")
	assert.Equal(t, []string{"event:flag-frontend-000001", "event:deploy-checkout-000002"}, rec.Truth.Distractors)
	assert.Equal(t, f.created, rec.SeriesFrom)
	assert.Equal(t, rec.DetectedAt.Add(time.Minute), rec.SeriesTo, "it keeps recording for the settle time")
	assert.Contains(t, f.query, `{namespace="gym-x", __name__=~"http_requests_total|`)
	assert.Contains(t, f.query, fmt.Sprintf("[%ds]", int(rec.SeriesTo.Sub(rec.SeriesFrom).Seconds())))

	// The files on disk.
	loaded, err := LoadRecordings(out)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, 2, loaded[0].Series)
	assert.Equal(t, 3, loaded[0].Samples)

	var events []Event
	b, err := os.ReadFile(filepath.Join(out, "x", FileEvents))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &events))
	require.Len(t, events, 2)
	assert.Equal(t, "feature_flag", events[0].Source)
	assert.Equal(t, zero.Add(-120*time.Second), events[0].Timestamp)
	assert.Equal(t, "deploy", events[1].Source)
	assert.Equal(t, zero.Add(60*time.Second), events[1].Timestamp)
	assert.Equal(t, events[0].Magnitude, events[1].Magnitude, "magnitude must not hint at the cause")

	gz, err := os.ReadFile(filepath.Join(out, "x", FileSeries))
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	require.NoError(t, err)
	om, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, `http_requests_total{code="201",namespace="gym-x",route="/api/v1/orders",service="frontend"} 10 1795600000
upstream_requests_total{code="502",namespace="gym-x",service="frontend",upstream="checkout"} 1 1795600000.123
upstream_requests_total{code="502",namespace="gym-x",service="frontend",upstream="checkout"} 7 1795600015.123
# EOF
`, string(om))

	// The repeating fault is stopped afterwards.
	assert.Contains(t, f.calls[len(f.calls)-1], "delete chaosexperiments.chaos.jamespham.io fault")
}

func TestRecordAControlResolvesTheCauseChange(t *testing.T) {
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	f := newFake(start)
	f.created = start.Add(-2 * time.Hour) // already warm, so no wait
	d := Definition{
		Name: "x", PagedService: "frontend",
		Changes: []Change{
			{Name: "flag", AtSeconds: -60, Kind: KindFeatureFlag, Service: "inventory", Flag: "f", Title: "inventory: flag f turned on"},
			{Name: "bad", AtSeconds: 0, Kind: KindDeploy, Service: "checkout", Title: "checkout v3", Set: map[string]string{"failureRate": "0.3"}},
		},
		Truth: TruthDef{Service: "checkout", Accept: []Rule{{Change: "bad"}}},
	}
	f.pageAfter = 0
	srv := f.prometheus(t, "gym-x")
	defer srv.Close()

	rec, err := newRecorder(f, srv.URL, t.TempDir()).Record(context.Background(), d)
	require.NoError(t, err)
	assert.Equal(t, []Rule{{Change: "bad", EvidenceID: "event:deploy-checkout-000002"}}, rec.Truth.Accept)
	assert.Equal(t, []string{"event:flag-inventory-000001"}, rec.Truth.Distractors)
	assert.Nil(t, rec.FaultAt)
	assert.Empty(t, rec.Truth.ChaosRuns)
	for _, c := range f.calls {
		assert.NotContains(t, c, "delete", "a control has no experiment to delete")
		assert.NotContains(t, c, "apply -f -")
	}
}

func TestRecordFailsWithoutAPage(t *testing.T) {
	f := newFake(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	f.pageAfter = time.Hour
	srv := f.prometheus(t, "gym-x")
	defer srv.Close()
	d := Definition{Name: "x", PagedService: "frontend",
		Fault: &Fault{Type: FaultNetworkLatency, Service: "inventory"},
		Truth: TruthDef{Service: "inventory", Accept: []Rule{{Service: "inventory"}}}}
	_, err := newRecorder(f, srv.URL, t.TempDir()).Record(context.Background(), d)
	require.ErrorContains(t, err, "no page within 10m0s")
	assert.Equal(t, "chaos-agent", f.applied["spec"].(map[string]any)["container"], "latency runs in the sidecar")
}

func TestRecordRefusesAShopThatIsAlreadyPaging(t *testing.T) {
	f := newFake(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	srv := f.prometheus(t, "gym-other")
	defer srv.Close()
	d := Definition{Name: "other", PagedService: "frontend",
		Fault: &Fault{Type: FaultNetworkLatency, Service: "inventory"},
		Truth: TruthDef{Service: "inventory", Accept: []Rule{{Service: "inventory"}}}}
	_, err := newRecorder(f, srv.URL, t.TempDir()).Record(context.Background(), d)
	require.ErrorContains(t, err, "already paging")
}

func TestScenarioFilesAreValid(t *testing.T) {
	defs, err := LoadDefinitions("../../gym/scenarios")
	require.NoError(t, err)
	assert.Len(t, defs, 5)
	for _, d := range defs {
		// Every scenario must page on frontend, the only service the page
		// rule in gym/shop.yaml watches.
		assert.Equal(t, "frontend", d.PagedService, d.Name)
	}
}

func TestValidateCatchesMistakes(t *testing.T) {
	d := Definition{
		Name: "Bad Name", PagedService: "web",
		Fault:   &Fault{Type: "disk-fill", Service: "db"},
		Changes: []Change{{Name: "a", Kind: "rollback", Service: "frontend"}, {Name: "a", Kind: KindFeatureFlag, Service: "frontend", Title: "t"}},
		Truth:   TruthDef{Service: "db", Accept: []Rule{{Change: "missing"}, {}}},
	}
	err := d.Validate()
	require.Error(t, err)
	for _, want := range []string{
		"name \"Bad Name\"", "paged_service \"web\"", "fault type \"disk-fill\"", "fault service \"db\"",
		"used twice", "has no title", "kind \"rollback\"", "needs a flag", "truth service \"db\"",
		"change \"missing\"", "must say something",
	} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestRecordWaitsForTheRunRecord(t *testing.T) {
	f := newFake(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	f.runsAfter = 8 * time.Minute
	srv := f.prometheus(t, "gym-x")
	defer srv.Close()
	d := Definition{Name: "x", PagedService: "frontend",
		Fault: &Fault{Type: FaultNetworkLatency, Service: "inventory"},
		Truth: TruthDef{Service: "inventory", Accept: []Rule{{Service: "inventory"}}}}
	rec, err := newRecorder(f, srv.URL, t.TempDir()).Record(context.Background(), d)
	require.NoError(t, err)
	assert.Len(t, rec.Truth.ChaosRuns, 1)
	assert.False(t, f.now.Before(f.faultAt.Add(8*time.Minute)), "it waited for the record")
	// The samples stop at page plus settle, not at the late run record.
	assert.Equal(t, rec.DetectedAt.Add(time.Minute), rec.SeriesTo)
}

func TestRecordFailsWhenTheFaultDidNotInject(t *testing.T) {
	f := newFake(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	f.outcome, f.injected = "failed", false
	srv := f.prometheus(t, "gym-x")
	defer srv.Close()
	d := Definition{Name: "x", PagedService: "frontend",
		Fault: &Fault{Type: FaultNetworkLatency, Service: "inventory"},
		Truth: TruthDef{Service: "inventory", Accept: []Rule{{Service: "inventory"}}}}
	_, err := newRecorder(f, srv.URL, t.TempDir()).Record(context.Background(), d)
	require.ErrorContains(t, err, "did not inject the fault: failed")
}

func TestRecordGivesUpWaitingForARunRecord(t *testing.T) {
	f := newFake(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC))
	f.runsAfter = time.Hour
	srv := f.prometheus(t, "gym-x")
	defer srv.Close()
	d := Definition{Name: "x", PagedService: "frontend",
		Fault: &Fault{Type: FaultNetworkLatency, Service: "inventory"},
		Truth: TruthDef{Service: "inventory", Accept: []Rule{{Service: "inventory"}}}}
	_, err := newRecorder(f, srv.URL, t.TempDir()).Record(context.Background(), d)
	require.ErrorContains(t, err, "no chaos run record within 15m0s")
}
