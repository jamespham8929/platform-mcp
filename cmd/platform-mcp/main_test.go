package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testdata(name string) string { return filepath.Join("..", "..", "testdata", name) }

func TestRunCommands(t *testing.T) {
	var out, errOut bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"version"}, &out, &errOut))
	assert.Equal(t, "dev\n", out.String())

	assert.ErrorContains(t, run(context.Background(), nil, &out, &errOut), "usage")
	assert.ErrorContains(t, run(context.Background(), []string{"frobnicate"}, &out, &errOut), "unknown command")
}

func TestServeConfig(t *testing.T) {
	cases := map[string]struct {
		flags serveFlags
		err   string
		tools []string
	}{
		"nothing":       {serveFlags{}, "nothing to serve", nil},
		"two chaos":     {serveFlags{chaosRuns: "x", chaosFromCluster: true}, "pass one", nil},
		"queries alone": {serveFlags{anomalyQueries: "q.json"}, "needs --prometheus-url", nil},
		"bad clock":     {serveFlags{events: multi{"e.json"}, now: "noon"}, "--now", nil},
		"events":        {serveFlags{events: multi{"e.json"}}, "", []string{"investigate"}},
		"everything": {serveFlags{
			chaosFromCluster: true, prometheusURL: "http://prom", anomalyQueries: "q.json",
		}, "", []string{"investigate", "chaos", "slo"}},
		"prometheus only": {serveFlags{prometheusURL: "http://prom"}, "", []string{"slo"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := tc.flags.config(nil)
			if tc.err != "" {
				assert.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			var got []string
			if cfg.Investigator != nil {
				got = append(got, "investigate")
			}
			if cfg.Chaos != nil {
				got = append(got, "chaos")
			}
			if cfg.SLO != nil {
				got = append(got, "slo")
			}
			assert.Equal(t, tc.tools, got)
		})
	}
}

func TestEvalFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"eval", "--scenarios", "does-not-exist"}, &out, &errOut)
	assert.ErrorContains(t, err, "loading scenarios")
	err = run(context.Background(), []string{"eval", "--scenarios", testdata("synthetic"), "--investigator", "oracle"}, &out, &errOut)
	assert.ErrorContains(t, err, "unknown investigator")
}

func incidentCLI(t *testing.T) string {
	bin := os.Getenv("INCIDENT_CLI")
	if bin == "" {
		t.Skip("set INCIDENT_CLI to an incident-cli binary")
	}
	return bin
}

func TestEvalBaselineWritesResults(t *testing.T) {
	bin := incidentCLI(t)
	outFile := filepath.Join(t.TempDir(), "results.json")
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{
		"eval", "--scenarios", testdata("synthetic"), "--incident-cli", bin,
		"--limit", "20", "--out", outFile,
	}, &out, &errOut)
	require.NoError(t, err, errOut.String())
	assert.Contains(t, out.String(), "baseline")

	var doc struct {
		Score struct {
			Scenarios        int     `json:"scenarios"`
			AgreesCorrelator float64 `json:"agrees_with_correlator"`
		} `json:"score"`
		Graded []json.RawMessage `json:"graded"`
	}
	b, err := os.ReadFile(outFile)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &doc))
	assert.Equal(t, 20, doc.Score.Scenarios)
	assert.Len(t, doc.Graded, 20)
	assert.Equal(t, 1.0, doc.Score.AgreesCorrelator)
}

// The server as an MCP client really starts it: a subprocess speaking the
// protocol on stdin and stdout, with logs on stderr.
func TestServeOverStdio(t *testing.T) {
	bin := incidentCLI(t)
	server := filepath.Join(t.TempDir(), "platform-mcp")
	build := exec.Command("go", "build", "-o", server, ".")
	build.Stderr = os.Stderr
	require.NoError(t, build.Run())

	cmd := exec.Command(server, "serve",
		"--incident-cli", bin,
		"--events", testdata("incident-example.json"),
		"--graph", testdata("service-dependencies.json"),
		"--chaos-runs", testdata("chaos-experiments.json"),
		"--now", "2026-01-15T12:00:00Z",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "test"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	require.NoError(t, err)
	defer cs.Close()

	list, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"investigate", "chaos_runs"}, names)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "investigate", Arguments: map[string]any{"service": "checkout", "top": 3},
	})
	require.NoError(t, err)
	require.False(t, res.IsError)
	b, _ := json.Marshal(res.StructuredContent)
	assert.Contains(t, string(b), `"evidence_id":"event:dep-checkout-4821"`)
	assert.Contains(t, string(b), `"evidence_id":"event:payments/checkout-nightly/20260115T114501Z"`,
		"the chaos run comes through incident-cli as an event")

	require.NoError(t, cs.Close())
	assert.True(t, strings.Contains(stderr.String(), "tool=investigate"), "calls are logged on stderr: %s", stderr.String())
}
