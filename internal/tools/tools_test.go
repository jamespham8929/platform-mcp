package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jamespham8929/platform-mcp/internal/chaos"
	"github.com/jamespham8929/platform-mcp/internal/incidentcli"
	"github.com/jamespham8929/platform-mcp/internal/promapi"
)

var incidentTime = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func testdata(name string) string { return filepath.Join("..", "..", "testdata", name) }

// connect serves cfg over an in-memory transport and returns a client
// session, so every test goes through the real MCP protocol: tool listing,
// schema validation of inputs and outputs, and error results.
func connect(t *testing.T, cfg Config) *mcp.ClientSession {
	t.Helper()
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return incidentTime }
	}
	server := NewServer(cfg, "test")
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	_, err := server.Connect(ctx, st, nil)
	require.NoError(t, err)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	if out != nil && !res.IsError {
		b, err := json.Marshal(res.StructuredContent)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, out))
	}
	return res
}

func errorText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// replay returns a runner that prints a recorded incident-cli output and
// records the arguments it was given.
func replay(t *testing.T, file string, gotArgs *[]string) *incidentcli.Runner {
	t.Helper()
	out, err := os.ReadFile(testdata(file))
	require.NoError(t, err)
	return &incidentcli.Runner{
		Sources: incidentcli.Sources{EventFiles: []string{"events.json"}},
		Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
			if gotArgs != nil {
				*gotArgs = args
			}
			return out, nil, nil
		},
	}
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		require.NotNil(t, tool.Annotations, tool.Name)
		assert.True(t, tool.Annotations.ReadOnlyHint, "%s must be marked read only", tool.Name)
	}
	return names
}

func TestOnlyConfiguredToolsAreListed(t *testing.T) {
	assert.Empty(t, toolNames(t, connect(t, Config{})))

	cs := connect(t, Config{Investigator: replay(t, "investigate-v1.json", nil)})
	assert.Equal(t, []string{"investigate"}, toolNames(t, cs))

	cs = connect(t, Config{
		Investigator: replay(t, "investigate-v1.json", nil),
		Chaos:        chaos.File{Path: testdata("chaos-experiments.json")},
		SLO:          promapi.New("http://unused"),
	})
	assert.ElementsMatch(t, []string{"investigate", "chaos_runs", "slo_state"}, toolNames(t, cs))
}

func TestInvestigateReturnsEvidence(t *testing.T) {
	var args []string
	cs := connect(t, Config{Investigator: replay(t, "investigate-v1.json", &args)})

	var out InvestigateOutput
	res := call(t, cs, "investigate", map[string]any{"service": "checkout", "top": 5}, &out)
	require.False(t, res.IsError, errorText(res))

	assert.Contains(t, args, "--service=checkout")
	assert.Contains(t, args, "--at=2026-01-15T12:00:00Z", "an empty at means the server clock")
	assert.Contains(t, args, "--window=2h0m0s")
	assert.Contains(t, args, "--top=5")
	assert.Contains(t, args, "--output=json")

	require.NotEmpty(t, out.Causes)
	assert.Equal(t, "event:dep-checkout-4821", out.Causes[0].EvidenceID)
	assert.InDelta(t, 8.0, out.Causes[0].LeadTimeMinutes, 1e-9)
	assert.Equal(t, "same service", out.Causes[0].Components.ServiceRelation)

	// Every cause cites an event that is in the same result.
	ids := map[string]bool{}
	for _, e := range out.Events {
		ids[e.EvidenceID] = true
	}
	for _, c := range out.Causes {
		assert.True(t, ids[c.EvidenceID], c.EvidenceID)
	}
	for _, e := range out.Events {
		if e.EvidenceID == "event:dep-checkout-4821" {
			assert.InDelta(t, 8.0, e.MinutesBefore, 1e-9)
		}
	}
}

func TestInvestigateRejectsBadInput(t *testing.T) {
	cs := connect(t, Config{Investigator: replay(t, "investigate-v1.json", nil)})
	cases := map[string]map[string]any{
		"flag as a service": {"service": "--graph=/etc/passwd"},
		"bad time":          {"service": "checkout", "at": "yesterday"},
		"huge window":       {"service": "checkout", "window_minutes": 10000},
		"too many causes":   {"service": "checkout", "top": 500},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := call(t, cs, "investigate", args, nil)
			assert.True(t, res.IsError, "expected a tool error")
		})
	}

	// A missing required field fails schema validation before the handler.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "investigate", Arguments: map[string]any{},
	})
	if err == nil {
		assert.True(t, res.IsError)
	}
}

func TestInvestigateReportsIncidentCLIFailure(t *testing.T) {
	runner := &incidentcli.Runner{
		Sources: incidentcli.Sources{EventFiles: []string{"x.json"}},
		Exec: func(context.Context, string, ...string) ([]byte, []byte, error) {
			return nil, []byte("no such file"), errors.New("exit status 1")
		},
	}
	res := call(t, connect(t, Config{Investigator: runner}), "investigate",
		map[string]any{"service": "checkout"}, nil)
	require.True(t, res.IsError)
	assert.Contains(t, errorText(res), "no such file")
}

func TestChaosRuns(t *testing.T) {
	cs := connect(t, Config{Chaos: chaos.File{Path: testdata("chaos-experiments.json")}})

	var out ChaosRunsOutput
	res := call(t, cs, "chaos_runs", map[string]any{"since_minutes": 120}, &out)
	require.False(t, res.IsError, errorText(res))
	require.Len(t, out.Runs, 2, "the 03:00 run is too old and the no_targets run injected nothing")
	assert.Equal(t, "chaos:identity/auth-latency/20260115T115000Z", out.Runs[0].EvidenceID, "newest first")
	assert.Equal(t, "aborted", out.Runs[0].Outcome)
	assert.Equal(t, "auth-latency", out.Runs[0].Experiment)

	res = call(t, cs, "chaos_runs", map[string]any{
		"service": "checkout", "since_minutes": 1440, "include_not_injected": true,
	}, &out)
	require.False(t, res.IsError, errorText(res))
	require.Len(t, out.Runs, 3)
	assert.False(t, out.Runs[1].Injected)
}

func TestChaosRunsReportsKubectlFailure(t *testing.T) {
	lister := chaos.Kubectl{Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New(`the server doesn't have a resource type "chaosexperiments"`)
	}}
	res := call(t, connect(t, Config{Chaos: lister}), "chaos_runs", map[string]any{}, nil)
	require.True(t, res.IsError)
	assert.Contains(t, errorText(res), "resource type")
}

// fakePrometheus answers instant queries of the form name{service="x"} from
// series that k8s-slo-tracker's real exporter produced. See
// scripts/slo-fixture.py.
func fakePrometheus(t *testing.T) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile(testdata("slo-tracker-series.json"))
	require.NoError(t, err)
	var fixture struct {
		Series []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"series"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/query", r.URL.Path)
		require.NoError(t, r.ParseForm())
		q := r.Form.Get("query")
		name, rest, _ := strings.Cut(q, "{")
		service := strings.TrimSuffix(strings.TrimPrefix(rest, `service="`), `"}`)
		result := []any{}
		for _, s := range fixture.Series {
			if s.Metric["__name__"] == name && s.Metric["service"] == service {
				result = append(result, map[string]any{"metric": s.Metric, "value": s.Value})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": result},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSLOState(t *testing.T) {
	prom := fakePrometheus(t)
	cs := connect(t, Config{
		SLO: promapi.New(prom.URL),
		Now: func() time.Time { return time.Date(2026, 1, 15, 11, 59, 0, 0, time.UTC) },
	})

	var out SLOStateOutput
	res := call(t, cs, "slo_state", map[string]any{"service": "checkout"}, &out)
	require.False(t, res.IsError, errorText(res))
	require.Len(t, out.SLOs, 2)

	avail := out.SLOs[0]
	assert.Equal(t, "slo:checkout/availability", avail.EvidenceID)
	assert.Equal(t, "critical", avail.Severity)
	assert.False(t, avail.Stale)
	assert.InDelta(t, 60, avail.SecondsSinceEval, 1e-6)
	require.Len(t, avail.Windows, 3)
	assert.Equal(t, []string{"5m", "1h", "6h"},
		[]string{avail.Windows[0].Window, avail.Windows[1].Window, avail.Windows[2].Window})
	assert.InDelta(t, 16.1, avail.Windows[1].BurnLowerBound, 1e-9)
	assert.InDelta(t, 19.2, avail.Windows[1].BurnObserved, 1e-9)
	assert.True(t, avail.Windows[1].SufficientData)

	latency := out.SLOs[1]
	assert.Equal(t, "insufficient_data", latency.Severity)
	assert.True(t, latency.Stale, "its last good evaluation was 16 minutes ago")
	assert.Equal(t, 1.0, latency.EvaluationErrors)
	assert.False(t, latency.Windows[0].SufficientData)
}

func TestSLOStateExplainsAnEmptyResult(t *testing.T) {
	cs := connect(t, Config{SLO: promapi.New(fakePrometheus(t).URL)})
	var out SLOStateOutput
	res := call(t, cs, "slo_state", map[string]any{"service": "billing"}, &out)
	require.False(t, res.IsError, errorText(res))
	assert.Empty(t, out.SLOs)
	assert.Contains(t, out.Note, "not healthy")
}

func TestSLOStateRejectsAnInjectedSelector(t *testing.T) {
	cs := connect(t, Config{SLO: promapi.New(fakePrometheus(t).URL)})
	res := call(t, cs, "slo_state", map[string]any{"service": `x"} or up{a="`}, nil)
	assert.True(t, res.IsError)
}

// When INCIDENT_CLI names a built incident-cli, the tool runs it for real.
// CI builds incident-cli from source for this, so a change to its JSON output
// breaks this test instead of breaking an investigation.
func TestInvestigateWithRealIncidentCLI(t *testing.T) {
	bin := os.Getenv("INCIDENT_CLI")
	if bin == "" {
		t.Skip("set INCIDENT_CLI to an incident-cli binary to run the contract test")
	}
	runner := &incidentcli.Runner{
		Binary: bin,
		Sources: incidentcli.Sources{
			EventFiles: []string{testdata("incident-example.json")},
			Graph:      testdata("service-dependencies.json"),
			ChaosRuns:  testdata("chaos-experiments.json"),
		},
	}
	cs := connect(t, Config{Investigator: runner})

	var out InvestigateOutput
	res := call(t, cs, "investigate", map[string]any{"service": "checkout", "top": 50}, &out)
	require.False(t, res.IsError, errorText(res))

	// The same ranking incident-cli's own golden file pins.
	golden, err := incidentcli.Parse(mustRead(t, testdata("investigate-v1.json")))
	require.NoError(t, err)
	require.Len(t, out.Causes, len(golden.Causes))
	for i, c := range golden.Causes {
		assert.Equal(t, EventEvidenceID(c.EventID), out.Causes[i].EvidenceID)
		assert.InDelta(t, c.Score, out.Causes[i].Score, 1e-9)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func TestTheWindowDefaultIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		configured, want int
	}{{0, 120}, {30, 30}} {
		var args []string
		cs := connect(t, Config{
			Investigator:         replay(t, "investigate-v1.json", &args),
			DefaultWindowMinutes: tc.configured,
		})
		list, err := cs.ListTools(context.Background(), nil)
		require.NoError(t, err)
		schema, _ := json.Marshal(list.Tools[0].InputSchema)
		assert.Contains(t, string(schema), fmt.Sprintf("10 to 360 minutes. Default %d.", tc.want),
			"the agent must be told the default it gets")

		var out InvestigateOutput
		call(t, cs, "investigate", map[string]any{"service": "checkout"}, &out)
		assert.Equal(t, tc.want, out.WindowMinutes)
		assert.Contains(t, args, "--window="+(time.Duration(tc.want)*time.Minute).String())
	}
}
