package gym

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jamespham8929/platform-mcp/internal/agent"
	"github.com/jamespham8929/platform-mcp/internal/chaos"
	"github.com/jamespham8929/platform-mcp/internal/eval"
	"github.com/jamespham8929/platform-mcp/internal/incidentcli"
	"github.com/jamespham8929/platform-mcp/internal/tools"
)

// DefaultWindowMinutes is the investigate window a replay offers by default.
// A gym shop has about 100 minutes of history, and the anomaly detector needs
// 30 of them before the window starts, so 120 would find nothing.
const DefaultWindowMinutes = 30

// Replayer runs investigators on recordings, with no cluster.
type Replayer struct {
	IncidentCLI string
	// Prometheus and Promtool are the binaries. The replay loads each
	// recording's samples into a fresh Prometheus.
	Prometheus string
	Promtool   string
	// Graph is the shop's dependency graph. Empty leaves it out.
	Graph string
	// AnomalyQueries is the incident-cli anomaly query file for the shop.
	AnomalyQueries       string
	DefaultWindowMinutes int
	// ShowChaos gives the investigator the chaos runs. They are the answer
	// key, so this is only for checking the pipe, never for a score.
	ShowChaos       bool
	MaxTotalCostUSD float64
	Log             *slog.Logger
	Progress        func(done, total int, g eval.Graded)
}

// Run investigates every recording and grades each answer.
func (r *Replayer) Run(ctx context.Context, recs []Loaded, inv agent.Investigator) ([]eval.Graded, error) {
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	var graded []eval.Graded
	spent := 0.0
	for i, rec := range recs {
		if r.MaxTotalCostUSD > 0 && spent >= r.MaxTotalCostUSD {
			return graded, fmt.Errorf("stopped after %d recordings: spent $%.2f of the $%.2f limit",
				i, spent, r.MaxTotalCostUSD)
		}
		g := r.one(ctx, rec, inv)
		if g.Result != nil {
			spent += g.Result.CostUSD
		}
		graded = append(graded, g)
		if r.Progress != nil {
			r.Progress(i+1, len(recs), g)
		}
	}
	return graded, nil
}

func (r *Replayer) one(ctx context.Context, rec Loaded, inv agent.Investigator) eval.Graded {
	g := eval.Graded{Scenario: rec.Definition.Name, Upstream: rec.Truth.Service != rec.Definition.PagedService}
	fail := func(err error) eval.Graded {
		g.Error = err.Error()
		return g
	}
	work, err := os.MkdirTemp("", "gym-replay-")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	promURL, stop, err := r.startPrometheus(ctx, rec, work)
	if err != nil {
		return fail(err)
	}
	defer stop()

	sources := incidentcli.Sources{
		EventFiles:     []string{rec.Path(FileEvents)},
		Graph:          r.Graph,
		PrometheusURL:  promURL,
		AnomalyQueries: r.AnomalyQueries,
	}
	cfg := tools.Config{
		Now:                  func() time.Time { return rec.DetectedAt },
		DefaultWindowMinutes: r.DefaultWindowMinutes,
	}
	if r.ShowChaos {
		sources.ChaosRuns = rec.Path(FileChaos)
		cfg.Chaos = chaos.File{Path: rec.Path(FileChaos)}
	}
	cfg.Investigator = &incidentcli.Runner{Binary: r.IncidentCLI, Sources: sources}

	res, err := eval.Investigate(ctx, cfg, inv, agent.Incident{
		Service:    rec.Definition.PagedService,
		DetectedAt: rec.DetectedAt,
		Summary:    pageSummary(rec.Alert),
	})
	g.Result = res
	if err != nil {
		return fail(err)
	}
	Grade(&g, rec.Recording, res)
	return g
}

// pageSummary is the alert text an on-call engineer would read.
func pageSummary(alert string) string {
	switch alert {
	case "GymPageErrors":
		return "More than 5% of frontend orders fail."
	case "GymPageLatency":
		return "frontend p99 order latency is above 300ms."
	}
	return alert
}

// startPrometheus loads the recording's samples into a new TSDB with promtool
// and serves it. It returns the URL and a function that stops the server.
func (r *Replayer) startPrometheus(ctx context.Context, rec Loaded, work string) (string, func(), error) {
	om := filepath.Join(work, "series.om")
	if err := gunzip(rec.Path(FileSeries), om); err != nil {
		return "", nil, err
	}
	data := filepath.Join(work, "data")
	if out, err := exec.CommandContext(ctx, r.Promtool, "tsdb", "create-blocks-from", "openmetrics", om, data).CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("promtool: %w: %s", err, strings.TrimSpace(string(out)))
	}
	config := filepath.Join(work, "prometheus.yml")
	if err := os.WriteFile(config, []byte("global: {}\n"), 0o600); err != nil {
		return "", nil, err
	}
	port, err := freePort()
	if err != nil {
		return "", nil, err
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	var logs strings.Builder
	cmd := exec.Command(r.Prometheus,
		"--config.file="+config,
		"--storage.tsdb.path="+data,
		// The samples are old. Without this, retention would drop them.
		"--storage.tsdb.retention.time=100y",
		"--web.listen-address="+addr,
		"--log.level=warn",
	)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	url := "http://" + addr
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(url + "/-/ready")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return url, stop, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			stop()
			return "", nil, fmt.Errorf("prometheus did not get ready: %s", strings.TrimSpace(logs.String()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func gunzip(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, zr); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}
