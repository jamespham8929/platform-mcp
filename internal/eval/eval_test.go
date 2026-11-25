package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jamespham8929/platform-mcp/internal/agent"
)

var suiteDir = filepath.Join("..", "..", "testdata", "synthetic")

func TestLoadSuite(t *testing.T) {
	s, err := Load(suiteDir)
	require.NoError(t, err)
	assert.Len(t, s.Scenarios, s.Manifest.Trials)
	for _, sc := range s.Scenarios {
		found := false
		for _, e := range sc.Events {
			found = found || e.ID == sc.Truth.EventID
			assert.NotEqual(t, "TRUE", e.ID)
		}
		assert.True(t, found, "%s: truth is not one of its events", sc.ID)
	}
}

// The baseline goes through MCP and the real incident-cli binary. It must
// rank every planted cause exactly where the benchmark ranked it. If it does
// not, the server or the contract is losing information, and every agent
// number built on top of it is suspect.
func TestBaselineReproducesTheBenchmark(t *testing.T) {
	bin := os.Getenv("INCIDENT_CLI")
	if bin == "" {
		t.Skip("set INCIDENT_CLI to an incident-cli binary to run the baseline eval")
	}
	suite, err := Load(suiteDir)
	require.NoError(t, err)

	for _, useGraph := range []bool{true, false} {
		graded, err := Run(context.Background(), suite,
			Setup{Name: "baseline", Investigator: agent.Baseline{}, UseGraph: useGraph},
			Options{IncidentCLI: bin})
		require.NoError(t, err)
		require.Len(t, graded, len(suite.Scenarios))
		for _, g := range graded {
			require.Empty(t, g.Error, g.Scenario)
			want := g.CorrelatorRank
			if want > agent.MaxCauses {
				want = 0
			}
			assert.Equal(t, want, g.Rank, "%s graph=%v", g.Scenario, useGraph)
			assert.Equal(t, 1.0, g.Citations.Share(), g.Scenario)
		}
		score := Summarize("baseline", "", graded)
		assert.Equal(t, 1.0, score.AgreesCorrelator)
	}
}

func TestSummarize(t *testing.T) {
	res := func(cost float64, d time.Duration, calls int) *agent.Result {
		return &agent.Result{CostUSD: cost, Duration: d, ToolCalls: calls, Stop: "answered"}
	}
	graded := []Graded{
		{Rank: 1, Upstream: true, CorrelatorRank: 1, Result: res(0.10, time.Second, 2),
			Citations: agent.Citations{Total: 2, Valid: 2}},
		{Rank: 2, CorrelatorRank: 1, Result: res(0.20, 3*time.Second, 4),
			Citations: agent.Citations{Total: 2, Valid: 1}},
		{Rank: 0, Abstained: true, CorrelatorRank: 5, Result: res(0.30, 2*time.Second, 1)},
		{Rank: 0, Upstream: true, CorrelatorRank: 2, Error: "boom",
			Citations: agent.Citations{Total: 1, Valid: 1, BadCauses: []string{"slo:x/y"}}},
	}
	s := Summarize("claude", "claude-opus-5-5", graded)
	assert.Equal(t, 4, s.Scenarios)
	assert.Equal(t, 1, s.Errors)
	assert.InDelta(t, 0.25, s.Top1, 1e-9)
	assert.InDelta(t, 0.50, s.Top3, 1e-9)
	assert.InDelta(t, (1+0.5)/4, s.MRR, 1e-9)
	assert.InDelta(t, 0.25, s.AbstainRate, 1e-9)
	assert.InDelta(t, 0.8, s.CitationValidity, 1e-9)
	assert.InDelta(t, 0.25, s.BadCauseRate, 1e-9)
	assert.InDelta(t, 0.5, s.UpstreamTop1, 1e-9)
	assert.InDelta(t, 0.0, s.SameServiceTop1, 1e-9)
	assert.InDelta(t, 0.5, s.AgreesCorrelator, 1e-9, "a correlator rank past 3 counts as 0")
	assert.InDelta(t, 0.60, s.CostUSD, 1e-9)
	assert.InDelta(t, 0.15, s.CostPerIncident, 1e-9)
	assert.Equal(t, 2*time.Second, s.MedianDuration)
	assert.Equal(t, 3, s.StopReasons["answered"])
}

func TestGradeMatchesEitherEvidenceKind(t *testing.T) {
	sc := Scenario{Truth: Truth{EventID: "payments/nightly/20260115T114501Z"}}
	for _, id := range []string{"chaos:payments/nightly/20260115T114501Z", "event:payments/nightly/20260115T114501Z"} {
		g := Graded{}
		grade(&g, sc, &agent.Result{
			Answer: &agent.Answer{Status: agent.StatusIdentified, Causes: []agent.Claim{{EvidenceID: "event:x"}, {EvidenceID: id}}},
			Seen:   map[string]bool{},
		})
		assert.Equal(t, 2, g.Rank, id)
	}

	g := Graded{}
	grade(&g, sc, &agent.Result{})
	assert.True(t, g.Abstained, "no answer at all counts as an abstention")
}

func TestRunStopsAtTheCostLimit(t *testing.T) {
	suite, err := Load(suiteDir)
	require.NoError(t, err)
	suite.Scenarios = suite.Scenarios[:3]
	graded, err := Run(context.Background(), suite,
		Setup{Name: "pricey", Investigator: pricey{}}, Options{MaxTotalCostUSD: 1.5})
	assert.ErrorContains(t, err, "stopped after 2 scenarios")
	assert.Len(t, graded, 2)
}

// pricey spends a dollar per incident and answers nothing.
type pricey struct{}

func (pricey) Name() string { return "pricey" }
func (pricey) Investigate(context.Context, *mcp.ClientSession, agent.Incident) (*agent.Result, error) {
	return &agent.Result{CostUSD: 1, Stop: "no_answer"}, nil
}
