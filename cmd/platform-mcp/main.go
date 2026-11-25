// Command platform-mcp serves read-only platform tools over MCP, and runs the
// offline eval that compares investigators on labeled incidents.
//
//	platform-mcp serve --events events.json --graph deps.json --chaos-from-cluster
//	platform-mcp eval --scenarios testdata/synthetic --investigator baseline
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jamespham8929/platform-mcp/internal/agent"
	"github.com/jamespham8929/platform-mcp/internal/chaos"
	"github.com/jamespham8929/platform-mcp/internal/eval"
	"github.com/jamespham8929/platform-mcp/internal/incidentcli"
	"github.com/jamespham8929/platform-mcp/internal/promapi"
	"github.com/jamespham8929/platform-mcp/internal/tools"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "platform-mcp:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  platform-mcp serve [flags]   serve the tools over MCP on stdin and stdout
  platform-mcp eval [flags]    score an investigator on labeled scenarios
  platform-mcp version
run a subcommand with -h for its flags`

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "eval":
		return runEval(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type serveFlags struct {
	incidentCLI      string
	events           multi
	graph            string
	chaosRuns        string
	chaosFromCluster bool
	kubeContext      string
	prometheusURL    string
	anomalyQueries   string
	githubRepos      multi
	now              string
}

func (f *serveFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.incidentCLI, "incident-cli", "incident", "incident-cli binary")
	fs.Var(&f.events, "events", "event file for investigate (repeatable)")
	fs.StringVar(&f.graph, "graph", "", "service dependency graph for investigate")
	fs.StringVar(&f.chaosRuns, "chaos-runs", "", "saved `kubectl get chaosexperiments -A -o json` output")
	fs.BoolVar(&f.chaosFromCluster, "chaos-from-cluster", false, "read chaos runs from the cluster with kubectl")
	fs.StringVar(&f.kubeContext, "kube-context", "", "kubeconfig context for --chaos-from-cluster")
	fs.StringVar(&f.prometheusURL, "prometheus-url", "", "Prometheus for slo_state, and for metric anomalies with --anomaly-queries")
	fs.StringVar(&f.anomalyQueries, "anomaly-queries", "", "incident-cli anomaly query file (needs --prometheus-url)")
	fs.Var(&f.githubRepos, "github-repo", "owner/repo[=service] to read deploys from (repeatable)")
	fs.StringVar(&f.now, "now", "", "fix the server clock to an RFC 3339 time, for replaying an incident")
}

func (f *serveFlags) config(logger *slog.Logger) (tools.Config, error) {
	cfg := tools.Config{Logger: logger}
	if f.chaosRuns != "" && f.chaosFromCluster {
		return cfg, errors.New("--chaos-runs and --chaos-from-cluster are two ways to get the same data; pass one")
	}
	if f.anomalyQueries != "" && f.prometheusURL == "" {
		return cfg, errors.New("--anomaly-queries needs --prometheus-url")
	}
	if f.now != "" {
		t, err := time.Parse(time.RFC3339, f.now)
		if err != nil {
			return cfg, fmt.Errorf("--now: %w", err)
		}
		cfg.Now = func() time.Time { return t }
	}

	sources := incidentcli.Sources{
		EventFiles:       f.events,
		Graph:            f.graph,
		ChaosRuns:        f.chaosRuns,
		ChaosFromCluster: f.chaosFromCluster,
		KubeContext:      f.kubeContext,
		GitHubRepos:      f.githubRepos,
	}
	if f.anomalyQueries != "" {
		sources.PrometheusURL, sources.AnomalyQueries = f.prometheusURL, f.anomalyQueries
	}
	if !sources.Empty() {
		cfg.Investigator = &incidentcli.Runner{Binary: f.incidentCLI, Sources: sources}
	}
	switch {
	case f.chaosRuns != "":
		cfg.Chaos = chaos.File{Path: f.chaosRuns}
	case f.chaosFromCluster:
		cfg.Chaos = chaos.Kubectl{Context: f.kubeContext}
	}
	if f.prometheusURL != "" {
		cfg.SLO = promapi.New(f.prometheusURL)
	}
	if cfg.Investigator == nil && cfg.Chaos == nil && cfg.SLO == nil {
		return cfg, errors.New("nothing to serve: configure at least one source")
	}
	return cfg, nil
}

func serve(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f serveFlags
	f.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// stdout carries MCP messages, so logs go to stderr.
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := f.config(logger)
	if err != nil {
		return err
	}
	return tools.NewServer(cfg, version).Run(ctx, &mcp.StdioTransport{})
}

func runEval(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaults := agent.DefaultConfig()
	scenarios := fs.String("scenarios", "testdata/synthetic", "scenario directory with a manifest.json")
	investigator := fs.String("investigator", "baseline", "baseline or claude")
	incidentCLI := fs.String("incident-cli", "incident", "incident-cli binary")
	noGraph := fs.Bool("no-graph", false, "do not give incident-cli the dependency graph")
	limit := fs.Int("limit", 0, "only the first N scenarios; 0 for all")
	out := fs.String("out", "", "write every graded result, with transcripts, to this JSON file")
	model := fs.String("model", defaults.Model, "Claude model for --investigator claude")
	effort := fs.String("effort", defaults.Effort, "effort: low, medium, high, xhigh or max")
	maxTurns := fs.Int("max-turns", defaults.MaxTurns, "request limit per investigation")
	maxCost := fs.Float64("max-cost-usd", defaults.MaxCostUSD, "estimated cost limit per investigation")
	maxTotal := fs.Float64("max-total-cost-usd", 5, "estimated cost limit for the whole run")
	if err := fs.Parse(args); err != nil {
		return err
	}

	suite, err := eval.Load(*scenarios)
	if err != nil {
		return fmt.Errorf("loading scenarios: %w", err)
	}
	if *limit > 0 && *limit < len(suite.Scenarios) {
		suite.Scenarios = suite.Scenarios[:*limit]
	}

	setup := eval.Setup{UseGraph: !*noGraph}
	modelID := ""
	switch *investigator {
	case "baseline":
		setup.Investigator = agent.Baseline{}
	case "claude":
		// The client reads ANTHROPIC_API_KEY or an `ant auth login` profile.
		client := anthropic.NewClient()
		cfg := agent.Config{
			Model: *model, Effort: *effort, MaxTurns: *maxTurns,
			MaxCostUSD: *maxCost, MaxTokens: defaults.MaxTokens,
		}
		setup.Investigator = &agent.Claude{Msgs: &client.Beta.Messages, Cfg: cfg}
		modelID = *model
	default:
		return fmt.Errorf("unknown investigator %q", *investigator)
	}
	setup.Name = *investigator
	if !setup.UseGraph {
		setup.Name += " (no graph)"
	}

	graded, runErr := eval.Run(ctx, suite, setup, eval.Options{
		IncidentCLI:     *incidentCLI,
		MaxTotalCostUSD: *maxTotal,
		Progress: func(done, total int, g eval.Graded) {
			if done%10 == 0 || done == total || g.Error != "" {
				fmt.Fprintf(stderr, "%d/%d %s rank=%d %s\n", done, total, g.Scenario, g.Rank, g.Error)
			}
		},
	})
	score := eval.Summarize(setup.Name, modelID, graded)
	eval.Print(stdout, []eval.Score{score})

	if *out != "" {
		doc := struct {
			Score  eval.Score    `json:"score"`
			Seed   int64         `json:"seed"`
			Graded []eval.Graded `json:"graded"`
		}{score, suite.Manifest.Seed, graded}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	if runErr != nil {
		return runErr
	}
	if score.Errors > 0 {
		return fmt.Errorf("%d of %d scenarios failed", score.Errors, score.Scenarios)
	}
	return nil
}
