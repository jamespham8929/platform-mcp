// Package tools defines the MCP tools platform-mcp serves. Every tool is read
// only. Each item a tool returns carries an evidence_id, and an investigator
// must cite those IDs for every claim it makes.
package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jamespham8929/platform-mcp/internal/chaos"
	"github.com/jamespham8929/platform-mcp/internal/incidentcli"
	"github.com/jamespham8929/platform-mcp/internal/slo"
)

// Config says which tools exist. A tool whose backend is nil is not
// registered, so an agent never sees a tool that can only fail.
type Config struct {
	Investigator *incidentcli.Runner
	Chaos        chaos.Lister
	SLO          slo.Querier
	// Now is the server clock. Replays set it to the incident time.
	Now    func() time.Time
	Logger *slog.Logger
}

const (
	maxEvents          = 200
	defaultTop         = 10
	maxTop             = 50
	defaultWindow      = 120
	minWindow          = 10
	maxWindow          = 360
	defaultChaosWindow = 180
	maxChaosWindow     = 1440
)

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

// NewServer builds the MCP server with the configured tools.
func NewServer(cfg Config, version string) *mcp.Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "platform-mcp", Version: version}, &mcp.ServerOptions{
		Instructions: "Read-only tools over one Kubernetes platform. Each result item has an " +
			"evidence_id. Cite those IDs for every claim. No tool changes anything.",
	})
	h := handlers{cfg}
	if cfg.Investigator != nil {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "investigate",
			Title:       "Rank likely causes of an incident",
			Annotations: readOnly,
			Description: "Runs incident-cli on the events this server is configured to read " +
				"(deploys, config changes, flags, scaling, alerts, metric anomalies, chaos runs) " +
				"and ranks which events most likely caused an incident on a service. Each cause " +
				"has a score and its parts: timing, service relevance, the base rate of its event " +
				"type, and magnitude. The ranking is a statistical baseline, not a verdict. It " +
				"knows nothing an event does not say.",
		}, logged(cfg.Logger, "investigate", h.investigate))
	}
	if cfg.Chaos != nil {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "chaos_runs",
			Title:       "List chaos experiment runs",
			Annotations: readOnly,
			Description: "Lists chaos-operator runs that started in a time range, newest first. " +
				"A run is a fault someone injected on purpose, such as pod failures or network " +
				"latency. By default only runs that actually touched a pod are listed.",
		}, logged(cfg.Logger, "chaos_runs", h.chaosRuns))
	}
	if cfg.SLO != nil {
		mcp.AddTool(s, &mcp.Tool{
			Name:        "slo_state",
			Title:       "Read SLO burn decisions",
			Annotations: readOnly,
			Description: "Reads k8s-slo-tracker's gated SLO decisions for a service from " +
				"Prometheus. For each SLO it returns the severity (critical, warning, none, or " +
				"insufficient_data), the burn rate and its lower confidence bound per window, and " +
				"whether the decision is stale. A lower bound above 1 means the budget is burning " +
				"faster than allowed, even after allowing for noise.",
		}, logged(cfg.Logger, "slo_state", h.sloState))
	}
	return s
}

// logged records each call on stderr. stdout carries the protocol, so it must
// stay clean.
func logged[In, Out any](log *slog.Logger, name string, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		res, out, err := h(ctx, req, in)
		attrs := []any{"tool", name, "ms", time.Since(start).Milliseconds()}
		if err != nil {
			log.Warn("tool call failed", append(attrs, "error", err)...)
		} else {
			log.Info("tool call", attrs...)
		}
		return res, out, err
	}
}

type handlers struct{ cfg Config }

func (h handlers) at(raw string) (time.Time, error) {
	if raw == "" {
		return h.cfg.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("at must be an RFC 3339 time such as 2026-01-15T12:00:00Z: %w", err)
	}
	return t.UTC(), nil
}

func clamp(v, def, lo, hi int) (int, error) {
	if v == 0 {
		return def, nil
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("must be between %d and %d, got %d", lo, hi, v)
	}
	return v, nil
}

// InvestigateInput is what the agent may choose. Sources are fixed by the
// server operator.
type InvestigateInput struct {
	Service       string `json:"service" jsonschema:"The service that paged, as alerts and deploys name it."`
	At            string `json:"at,omitempty" jsonschema:"When the incident was detected, in RFC 3339. Empty means now."`
	WindowMinutes int    `json:"window_minutes,omitempty" jsonschema:"How far back to look for causes, 10 to 360 minutes. Default 120."`
	Top           int    `json:"top,omitempty" jsonschema:"How many ranked causes to return, 1 to 50. Default 10."`
}

type RankedCause struct {
	Rank            int                    `json:"rank"`
	EvidenceID      string                 `json:"evidence_id"`
	Score           float64                `json:"score"`
	LeadTimeMinutes float64                `json:"lead_time_minutes"`
	Components      incidentcli.Components `json:"components"`
}

type EventEvidence struct {
	EvidenceID string    `json:"evidence_id"`
	Source     string    `json:"source"`
	Service    string    `json:"service"`
	Title      string    `json:"title"`
	Timestamp  time.Time `json:"timestamp"`
	// MinutesBefore is negative for events after detection.
	MinutesBefore float64           `json:"minutes_before_detection"`
	Magnitude     float64           `json:"magnitude"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type InvestigateOutput struct {
	Service         string          `json:"service"`
	DetectedAt      time.Time       `json:"detected_at"`
	WindowMinutes   int             `json:"window_minutes"`
	Sources         []string        `json:"sources"`
	Causes          []RankedCause   `json:"causes"`
	TotalCandidates int             `json:"total_candidates"`
	Events          []EventEvidence `json:"events"`
	EventsTruncated bool            `json:"events_truncated"`
}

// EventEvidenceID is the evidence ID for an incident-cli event.
func EventEvidenceID(eventID string) string { return "event:" + eventID }

func (h handlers) investigate(ctx context.Context, _ *mcp.CallToolRequest, in InvestigateInput) (*mcp.CallToolResult, InvestigateOutput, error) {
	var out InvestigateOutput
	at, err := h.at(in.At)
	if err != nil {
		return nil, out, err
	}
	window, err := clamp(in.WindowMinutes, defaultWindow, minWindow, maxWindow)
	if err != nil {
		return nil, out, fmt.Errorf("window_minutes %w", err)
	}
	top, err := clamp(in.Top, defaultTop, 1, maxTop)
	if err != nil {
		return nil, out, fmt.Errorf("top %w", err)
	}
	inv, err := h.cfg.Investigator.Investigate(ctx, incidentcli.Request{
		Service: in.Service, At: at, Window: time.Duration(window) * time.Minute, Top: top,
	})
	if err != nil {
		return nil, out, err
	}

	out = InvestigateOutput{
		Service:         inv.Incident.Service,
		DetectedAt:      inv.Incident.DetectedAt,
		WindowMinutes:   window,
		Sources:         inv.Sources,
		Causes:          make([]RankedCause, 0, len(inv.Causes)),
		TotalCandidates: inv.TotalCandidates,
		Events:          make([]EventEvidence, 0, len(inv.Events)),
	}
	for _, c := range inv.Causes {
		out.Causes = append(out.Causes, RankedCause{
			Rank:            c.Rank,
			EvidenceID:      EventEvidenceID(c.EventID),
			Score:           c.Score,
			LeadTimeMinutes: c.LeadTimeSeconds / 60,
			Components:      c.Components,
		})
	}
	// Keep the newest events when a busy hour has too many. The ranked
	// causes are always listed above, so none of them is lost.
	events := inv.Events
	if len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
		out.EventsTruncated = true
	}
	for _, e := range events {
		out.Events = append(out.Events, EventEvidence{
			EvidenceID:    EventEvidenceID(e.ID),
			Source:        e.Source,
			Service:       e.Service,
			Title:         e.Title,
			Timestamp:     e.Timestamp,
			MinutesBefore: -e.OffsetSeconds / 60,
			Magnitude:     e.Magnitude,
			Metadata:      e.Metadata,
		})
	}
	return nil, out, nil
}

type ChaosRunsInput struct {
	Service            string `json:"service,omitempty" jsonschema:"Only runs that targeted this service. Empty means every service."`
	At                 string `json:"at,omitempty" jsonschema:"End of the time range, in RFC 3339. Empty means now."`
	SinceMinutes       int    `json:"since_minutes,omitempty" jsonschema:"Length of the time range, 10 to 1440 minutes. Default 180."`
	IncludeNotInjected bool   `json:"include_not_injected,omitempty" jsonschema:"Also list runs that stopped before touching any pod."`
}

type ChaosRunEvidence struct {
	EvidenceID string `json:"evidence_id"`
	chaos.Run
}

type ChaosRunsOutput struct {
	From time.Time          `json:"from"`
	To   time.Time          `json:"to"`
	Runs []ChaosRunEvidence `json:"runs"`
}

// ChaosEvidenceID is the evidence ID for a chaos run.
func ChaosEvidenceID(runID string) string { return "chaos:" + runID }

func (h handlers) chaosRuns(ctx context.Context, _ *mcp.CallToolRequest, in ChaosRunsInput) (*mcp.CallToolResult, ChaosRunsOutput, error) {
	var out ChaosRunsOutput
	if in.Service != "" && !incidentcli.ValidService(in.Service) {
		return nil, out, fmt.Errorf("invalid service name %q", in.Service)
	}
	to, err := h.at(in.At)
	if err != nil {
		return nil, out, err
	}
	since, err := clamp(in.SinceMinutes, defaultChaosWindow, minWindow, maxChaosWindow)
	if err != nil {
		return nil, out, fmt.Errorf("since_minutes %w", err)
	}
	raw, err := h.cfg.Chaos.List(ctx)
	if err != nil {
		return nil, out, fmt.Errorf("listing chaos experiments: %w", err)
	}
	runs, err := chaos.Parse(raw)
	if err != nil {
		return nil, out, err
	}
	from := to.Add(-time.Duration(since) * time.Minute)
	out = ChaosRunsOutput{From: from, To: to, Runs: []ChaosRunEvidence{}}
	for _, r := range chaos.Filter(runs, in.Service, from, to, in.IncludeNotInjected) {
		out.Runs = append(out.Runs, ChaosRunEvidence{EvidenceID: ChaosEvidenceID(r.ID), Run: r})
	}
	return nil, out, nil
}

type SLOStateInput struct {
	Service string `json:"service" jsonschema:"The service whose SLOs to read."`
	At      string `json:"at,omitempty" jsonschema:"Time to read the decision at, in RFC 3339. Empty means now."`
}

type SLOStateOutput struct {
	Service string      `json:"service"`
	At      time.Time   `json:"at"`
	SLOs    []slo.State `json:"slos"`
	// Note explains an empty result, which is easy to misread as healthy.
	Note string `json:"note,omitempty"`
}

func (h handlers) sloState(ctx context.Context, _ *mcp.CallToolRequest, in SLOStateInput) (*mcp.CallToolResult, SLOStateOutput, error) {
	var out SLOStateOutput
	if !incidentcli.ValidService(in.Service) {
		return nil, out, fmt.Errorf("invalid service name %q", in.Service)
	}
	at, err := h.at(in.At)
	if err != nil {
		return nil, out, err
	}
	states, err := slo.Read(ctx, h.cfg.SLO, in.Service, at)
	if err != nil {
		return nil, out, errors.Join(errors.New("reading SLO state from prometheus"), err)
	}
	out = SLOStateOutput{Service: in.Service, At: at, SLOs: states}
	if len(states) == 0 {
		out.Note = "slo-tracker exports no SLOs for this service. That means no data, not healthy."
	}
	return nil, out, nil
}
