// Package incidentcli runs `incident investigate --output json` and parses the
// result. The output format is a versioned contract that incident-cli
// documents in docs/investigate-json.md. This package reads version 1 and
// refuses anything else.
package incidentcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// Schema is the only output version this package understands.
const Schema = "incident-cli/investigate/v1"

// Investigation mirrors incident-cli's JSON output. Unknown fields are
// ignored, as the contract asks.
type Investigation struct {
	Schema   string `json:"schema"`
	Incident struct {
		Service    string    `json:"service"`
		DetectedAt time.Time `json:"detected_at"`
	} `json:"incident"`
	WindowSeconds   int64    `json:"window_seconds"`
	Sources         []string `json:"sources"`
	Events          []Event  `json:"events"`
	Causes          []Cause  `json:"causes"`
	TotalCandidates int      `json:"total_candidates"`
}

type Event struct {
	ID            string            `json:"id"`
	Source        string            `json:"source"`
	Service       string            `json:"service"`
	Title         string            `json:"title"`
	Timestamp     time.Time         `json:"timestamp"`
	OffsetSeconds float64           `json:"offset_seconds"`
	Magnitude     float64           `json:"magnitude"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type Cause struct {
	Rank            int        `json:"rank"`
	EventID         string     `json:"event_id"`
	Score           float64    `json:"score"`
	LeadTimeSeconds float64    `json:"lead_time_seconds"`
	Components      Components `json:"components"`
}

type Components struct {
	Temporal        float64 `json:"temporal"`
	Service         float64 `json:"service"`
	TypePrior       float64 `json:"type_prior"`
	Magnitude       float64 `json:"magnitude"`
	Relevance       float64 `json:"relevance"`
	ServiceRelation string  `json:"service_relation,omitempty"`
}

// Sources is what the server operator configured. The agent never chooses
// files, repos or URLs. It only picks the service, the time and the window.
type Sources struct {
	EventFiles       []string
	Graph            string
	ChaosRuns        string
	ChaosFromCluster bool
	KubeContext      string
	PrometheusURL    string
	AnomalyQueries   string
	GitHubRepos      []string
}

// Empty reports whether no source is configured, which incident-cli rejects.
func (s Sources) Empty() bool {
	return len(s.EventFiles) == 0 && s.ChaosRuns == "" && !s.ChaosFromCluster &&
		s.AnomalyQueries == "" && len(s.GitHubRepos) == 0
}

func (s Sources) args() []string {
	var args []string
	for _, f := range s.EventFiles {
		args = append(args, "--events="+f)
	}
	if s.Graph != "" {
		args = append(args, "--graph="+s.Graph)
	}
	if s.ChaosRuns != "" {
		args = append(args, "--chaos-runs="+s.ChaosRuns)
	}
	if s.ChaosFromCluster {
		args = append(args, "--chaos-from-cluster")
		if s.KubeContext != "" {
			args = append(args, "--kube-context="+s.KubeContext)
		}
	}
	if s.PrometheusURL != "" || s.AnomalyQueries != "" {
		args = append(args, "--prometheus-url="+s.PrometheusURL, "--anomaly-queries="+s.AnomalyQueries)
	}
	for _, r := range s.GitHubRepos {
		args = append(args, "--github-repo="+r)
	}
	return args
}

// Request is what the agent may vary between calls.
type Request struct {
	Service string
	At      time.Time
	Window  time.Duration
	Top     int // 0 keeps every candidate
}

// serviceName limits service names to what Kubernetes allows in a label
// value. It also keeps a value from ever looking like a flag.
var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidService reports whether s is safe to pass as a service name.
func ValidService(s string) bool { return serviceName.MatchString(s) }

// Runner executes incident-cli.
type Runner struct {
	// Binary is the incident-cli executable. Empty means "incident" on PATH.
	Binary  string
	Sources Sources
	// Timeout bounds one investigation. GitHub and Prometheus sources make
	// network calls, so this is a real limit, not a formality.
	Timeout time.Duration
	// Exec runs the command and returns stdout and stderr. Nil means os/exec.
	// Tests swap it to replay a recorded output.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, []byte, error)
}

func (r *Runner) binary() string {
	if r.Binary == "" {
		return "incident"
	}
	return r.Binary
}

// Args builds the command line for a request. It is exported for tests and
// for logging what ran.
func (r *Runner) Args(req Request) ([]string, error) {
	if !ValidService(req.Service) {
		return nil, fmt.Errorf("invalid service name %q", req.Service)
	}
	if req.Window <= 0 {
		return nil, errors.New("window must be positive")
	}
	if req.Top < 0 {
		return nil, errors.New("top must be 0 or more")
	}
	if r.Sources.Empty() {
		return nil, errors.New("the server has no event sources configured")
	}
	args := []string{
		"investigate",
		"--service=" + req.Service,
		"--at=" + req.At.UTC().Format(time.RFC3339),
		"--window=" + req.Window.String(),
		"--top=" + strconv.Itoa(req.Top),
		"--output=json",
	}
	return append(args, r.Sources.args()...), nil
}

// Investigate runs one investigation and checks the output version.
func (r *Runner) Investigate(ctx context.Context, req Request) (*Investigation, error) {
	args, err := r.Args(req)
	if err != nil {
		return nil, err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	run := r.Exec
	if run == nil {
		run = runCommand
	}
	stdout, stderr, err := run(ctx, r.binary(), args...)
	if err != nil {
		if msg := bytes.TrimSpace(stderr); len(msg) > 0 {
			return nil, fmt.Errorf("incident-cli failed: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("incident-cli failed: %w", err)
	}
	return Parse(stdout)
}

// Parse reads one investigation document and refuses an unknown version.
func Parse(b []byte) (*Investigation, error) {
	var inv Investigation
	if err := json.Unmarshal(b, &inv); err != nil {
		return nil, fmt.Errorf("parsing incident-cli output: %w", err)
	}
	if inv.Schema != Schema {
		return nil, fmt.Errorf("incident-cli printed schema %q, this server reads %q", inv.Schema, Schema)
	}
	return &inv, nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
