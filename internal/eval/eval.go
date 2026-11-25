// Package eval runs an investigator over labeled scenarios and scores it.
//
// Phase 1 uses the synthetic scenarios that incident-cli's benchmark exports.
// Each scenario is a set of events with one planted cause. The grader knows
// the cause. The investigator never sees it.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jamespham8929/platform-mcp/internal/agent"
	"github.com/jamespham8929/platform-mcp/internal/incidentcli"
	"github.com/jamespham8929/platform-mcp/internal/tools"
)

type Event struct {
	ID        string    `json:"id"`
	Source    string    `json:"source"`
	Service   string    `json:"service"`
	Title     string    `json:"title"`
	Timestamp time.Time `json:"timestamp"`
	Magnitude float64   `json:"magnitude"`
}

type Truth struct {
	EventID        string `json:"event_id"`
	Service        string `json:"service"`
	Upstream       bool   `json:"upstream"`
	CorrelatorRank struct {
		Exact int `json:"exact"`
		Graph int `json:"graph"`
	} `json:"correlator_rank"`
}

type Scenario struct {
	ID            string    `json:"id"`
	Service       string    `json:"service"`
	DetectedAt    time.Time `json:"detected_at"`
	WindowSeconds int64     `json:"window_seconds"`
	Events        []Event   `json:"events"`
	Truth         Truth     `json:"truth"`
}

type Manifest struct {
	GeneratedBy string   `json:"generated_by"`
	Seed        int64    `json:"seed"`
	Trials      int      `json:"trials"`
	Graph       string   `json:"graph"`
	Scenarios   []string `json:"scenarios"`
}

// Suite is a loaded scenario directory.
type Suite struct {
	Dir       string
	Manifest  Manifest
	Scenarios []Scenario
}

func Load(dir string) (*Suite, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	s := &Suite{Dir: dir}
	if err := json.Unmarshal(b, &s.Manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	for _, name := range s.Manifest.Scenarios {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var sc Scenario
		if err := json.Unmarshal(b, &sc); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", name, err)
		}
		s.Scenarios = append(s.Scenarios, sc)
	}
	return s, nil
}

// Setup is one arm of the comparison.
type Setup struct {
	Name         string
	Investigator agent.Investigator
	// UseGraph hands incident-cli the dependency graph. Without it the
	// ranking only knows exact service matches.
	UseGraph bool
}

type Options struct {
	IncidentCLI string
	// MaxTotalCostUSD stops the run once the estimated cost passes it.
	MaxTotalCostUSD float64
	// Progress is called after each scenario. It may be nil.
	Progress func(done, total int, g Graded)
}

// Graded is one scenario's outcome.
type Graded struct {
	Scenario  string          `json:"scenario"`
	Upstream  bool            `json:"upstream"`
	Rank      int             `json:"rank"` // 0 when the cause is not in the answer
	Abstained bool            `json:"abstained"`
	Citations agent.Citations `json:"citations"`
	// CorrelatorRank is the benchmark's own rank, for a per-scenario diff.
	CorrelatorRank int `json:"correlator_rank"`
	// Distracted is true when the top cause was a change the scenario
	// planted as a distraction. Only the gym plants them.
	Distracted bool          `json:"distracted,omitempty"`
	Result     *agent.Result `json:"result,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// Run investigates every scenario with one setup and grades each answer.
func Run(ctx context.Context, suite *Suite, setup Setup, opts Options) ([]Graded, error) {
	work, err := os.MkdirTemp("", "platform-mcp-eval-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	var graded []Graded
	spent := 0.0
	for i, sc := range suite.Scenarios {
		if opts.MaxTotalCostUSD > 0 && spent >= opts.MaxTotalCostUSD {
			return graded, fmt.Errorf("stopped after %d scenarios: spent $%.2f of the $%.2f limit",
				i, spent, opts.MaxTotalCostUSD)
		}
		g := runOne(ctx, suite, sc, setup, opts, work)
		if g.Result != nil {
			spent += g.Result.CostUSD
		}
		graded = append(graded, g)
		if opts.Progress != nil {
			opts.Progress(i+1, len(suite.Scenarios), g)
		}
	}
	return graded, nil
}

func runOne(ctx context.Context, suite *Suite, sc Scenario, setup Setup, opts Options, work string) Graded {
	g := Graded{Scenario: sc.ID, Upstream: sc.Truth.Upstream, CorrelatorRank: sc.Truth.CorrelatorRank.Exact}
	if setup.UseGraph {
		g.CorrelatorRank = sc.Truth.CorrelatorRank.Graph
	}

	// The investigator gets the events only. The truth block stays here.
	eventsPath := filepath.Join(work, sc.ID+".json")
	b, err := json.Marshal(sc.Events)
	if err != nil {
		g.Error = err.Error()
		return g
	}
	if err := os.WriteFile(eventsPath, b, 0o600); err != nil {
		g.Error = err.Error()
		return g
	}
	sources := incidentcli.Sources{EventFiles: []string{eventsPath}}
	if setup.UseGraph {
		sources.Graph = filepath.Join(suite.Dir, suite.Manifest.Graph)
	}

	res, err := Investigate(ctx, tools.Config{
		Investigator: &incidentcli.Runner{Binary: opts.IncidentCLI, Sources: sources},
		// The server clock is the incident time, so "now" means then.
		Now: func() time.Time { return sc.DetectedAt },
	}, setup.Investigator, agent.Incident{Service: sc.Service, DetectedAt: sc.DetectedAt})
	g.Result = res
	if err != nil {
		g.Error = err.Error()
		return g
	}
	grade(&g, sc, res)
	return g
}

// Investigate serves the tools in cfg over an in-memory MCP session and lets
// the investigator use them, the same way a real MCP client would.
func Investigate(ctx context.Context, cfg tools.Config, inv agent.Investigator, inc agent.Incident) (*agent.Result, error) {
	server := tools.NewServer(cfg, "eval")
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ss.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "eval", Version: "eval"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cs.Close() }()
	return inv.Investigate(ctx, cs, inc)
}

func grade(g *Graded, sc Scenario, res *agent.Result) {
	if res.Answer == nil {
		g.Abstained = true
		return
	}
	g.Citations = agent.CheckCitations(*res.Answer, res.Seen)
	if res.Answer.Status == agent.StatusInsufficient {
		g.Abstained = true
	}
	for i, c := range res.Answer.Causes {
		if agent.UnderlyingID(c.EvidenceID) == sc.Truth.EventID {
			g.Rank = i + 1
			break
		}
	}
}
