package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/jamespham8929/platform-mcp/internal/agent"
	"github.com/jamespham8929/platform-mcp/internal/eval"
	"github.com/jamespham8929/platform-mcp/internal/gym"
)

func runGym(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: platform-mcp gym record|replay [flags]")
	}
	switch args[0] {
	case "record":
		return gymRecord(ctx, args[1:], stderr)
	case "replay":
		return gymReplay(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown gym command %q, want record or replay", args[0])
	}
}

func gymRecord(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("gym record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scenarios := fs.String("scenarios", "gym/scenarios", "scenario directory")
	out := fs.String("out", "gym/recordings", "where to write the recordings")
	only := fs.String("only", "", "record only this scenario")
	kubeContext := fs.String("kube-context", "kind-gym", "kubeconfig context of the gym cluster")
	promURL := fs.String("prometheus-url", "http://127.0.0.1:19090", "the gym Prometheus, usually port-forwarded")
	chart := fs.String("chart", "../gitops-platform-demo/charts/sample-service", "gitops-platform-demo's sample-service chart")
	warmup := fs.Duration("warmup", 95*time.Minute, "how long each shop must run before its scenario starts")
	pageTimeout := fs.Duration("page-timeout", 10*time.Minute, "how long to wait for the page")
	settle := fs.Duration("settle", time.Minute, "how long to keep recording after the page")
	stagger := fs.Duration("stagger", time.Minute, "time between scenario starts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	defs, err := gym.LoadDefinitions(*scenarios)
	if err != nil {
		return err
	}
	if *only != "" {
		var picked []gym.Definition
		for _, d := range defs {
			if d.Name == *only {
				picked = append(picked, d)
			}
		}
		if len(picked) == 0 {
			return fmt.Errorf("no scenario named %q", *only)
		}
		defs = picked
	}
	rec := &gym.Recorder{
		Prom:        gym.Prom{BaseURL: *promURL},
		KubeContext: *kubeContext,
		Chart:       *chart,
		Out:         *out,
		Warmup:      *warmup,
		PageTimeout: *pageTimeout,
		Settle:      *settle,
		Log:         slog.New(slog.NewTextHandler(stderr, nil)),
	}
	return rec.RecordAll(ctx, defs, *stagger)
}

func gymReplay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gym replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaults := agent.DefaultConfig()
	recordings := fs.String("recordings", "gym/recordings", "recordings directory")
	gymDir := fs.String("gym", "gym", "gym directory with graph.json and anomaly-queries.json")
	incidentCLI := fs.String("incident-cli", "incident", "incident-cli binary")
	prometheus := fs.String("prometheus", "prometheus", "prometheus binary")
	promtool := fs.String("promtool", "promtool", "promtool binary")
	investigator := fs.String("investigator", "baseline", "baseline or claude")
	noGraph := fs.Bool("no-graph", false, "do not give incident-cli the dependency graph")
	showChaos := fs.Bool("show-chaos", false, "give the investigator the chaos runs, which are the answer key; for checking the pipe only")
	window := fs.Int("window", gym.DefaultWindowMinutes, "default investigate window in minutes")
	out := fs.String("out", "", "write every graded result, with transcripts, to this JSON file")
	model := fs.String("model", defaults.Model, "Claude model for --investigator claude")
	effort := fs.String("effort", defaults.Effort, "effort: low, medium, high, xhigh or max")
	maxTurns := fs.Int("max-turns", defaults.MaxTurns, "request limit per investigation")
	maxCost := fs.Float64("max-cost-usd", defaults.MaxCostUSD, "estimated cost limit per investigation")
	maxTotal := fs.Float64("max-total-cost-usd", 5, "estimated cost limit for the whole run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	recs, err := gym.LoadRecordings(*recordings)
	if err != nil {
		return err
	}
	inv, modelID, err := newInvestigator(*investigator, agent.Config{
		Model: *model, Effort: *effort, MaxTurns: *maxTurns,
		MaxCostUSD: *maxCost, MaxTokens: defaults.MaxTokens,
	})
	if err != nil {
		return err
	}
	r := &gym.Replayer{
		IncidentCLI: *incidentCLI, Prometheus: *prometheus, Promtool: *promtool,
		AnomalyQueries:       filepath.Join(*gymDir, "anomaly-queries.json"),
		DefaultWindowMinutes: *window,
		ShowChaos:            *showChaos,
		MaxTotalCostUSD:      *maxTotal,
		Log:                  slog.New(slog.NewTextHandler(stderr, nil)),
	}
	name := *investigator
	if !*noGraph {
		r.Graph = filepath.Join(*gymDir, "graph.json")
	} else {
		name += " (no graph)"
	}
	if *showChaos {
		name += " (chaos visible)"
	}
	graded, runErr := r.Run(ctx, recs, inv)
	score := eval.Summarize(name, modelID, graded)
	gym.PrintScore(stdout, score)
	fmt.Fprintln(stdout)
	gym.PrintScenarios(stdout, recs, graded)
	if *out != "" {
		if err := writeResults(*out, score, 0, graded); err != nil {
			return err
		}
	}
	if runErr != nil {
		return runErr
	}
	if score.Errors > 0 {
		return fmt.Errorf("%d of %d recordings failed", score.Errors, score.Scenarios)
	}
	return nil
}
