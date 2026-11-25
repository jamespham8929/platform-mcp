package incidentcli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var at = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func TestArgs(t *testing.T) {
	r := &Runner{Sources: Sources{
		EventFiles: []string{"a.json", "b.json"}, Graph: "g.json",
		ChaosFromCluster: true, KubeContext: "kind",
		PrometheusURL: "http://prom:9090", AnomalyQueries: "q.json",
		GitHubRepos: []string{"org/checkout=checkout"},
	}}
	args, err := r.Args(Request{Service: "checkout", At: at, Window: 90 * time.Minute, Top: 0})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"investigate", "--service=checkout", "--at=2026-01-15T12:00:00Z", "--window=1h30m0s",
		"--top=0", "--output=json",
		"--events=a.json", "--events=b.json", "--graph=g.json",
		"--chaos-from-cluster", "--kube-context=kind",
		"--prometheus-url=http://prom:9090", "--anomaly-queries=q.json",
		"--github-repo=org/checkout=checkout",
	}, args)
}

func TestArgsRejects(t *testing.T) {
	r := &Runner{Sources: Sources{EventFiles: []string{"a.json"}}}
	for _, svc := range []string{"", "-x", "--graph=/etc/passwd", "a b", "checkout;rm", "x\"y"} {
		_, err := r.Args(Request{Service: svc, At: at, Window: time.Hour})
		assert.Error(t, err, "%q", svc)
	}
	_, err := r.Args(Request{Service: "ok", At: at})
	assert.Error(t, err, "no window")
	_, err = r.Args(Request{Service: "ok", At: at, Window: time.Hour, Top: -1})
	assert.Error(t, err)
	_, err = (&Runner{}).Args(Request{Service: "ok", At: at, Window: time.Hour})
	assert.ErrorContains(t, err, "no event sources")
}

func TestParse(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "investigate-v1.json"))
	require.NoError(t, err)
	inv, err := Parse(b)
	require.NoError(t, err)
	assert.Equal(t, "checkout", inv.Incident.Service)
	assert.Equal(t, inv.TotalCandidates, len(inv.Causes))
	assert.Equal(t, "dep-checkout-4821", inv.Causes[0].EventID)

	_, err = Parse([]byte(`{"schema":"incident-cli/investigate/v2"}`))
	assert.ErrorContains(t, err, "v2", "a new version is refused, not misread")
	_, err = Parse([]byte(`not json`))
	assert.Error(t, err)
}

func TestInvestigateErrors(t *testing.T) {
	r := &Runner{
		Sources: Sources{EventFiles: []string{"a.json"}},
		Exec: func(context.Context, string, ...string) ([]byte, []byte, error) {
			return nil, []byte("  open a.json: no such file  \n"), errors.New("exit status 1")
		},
	}
	_, err := r.Investigate(context.Background(), Request{Service: "checkout", At: at, Window: time.Hour})
	assert.ErrorContains(t, err, "open a.json: no such file")

	r.Exec = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("signal: killed")
	}
	_, err = r.Investigate(context.Background(), Request{Service: "checkout", At: at, Window: time.Hour})
	assert.ErrorContains(t, err, "killed")
}

func TestInvestigateUsesTheBinaryAndATimeout(t *testing.T) {
	var name string
	var deadline bool
	r := &Runner{
		Sources: Sources{EventFiles: []string{"a.json"}},
		Exec: func(ctx context.Context, n string, _ ...string) ([]byte, []byte, error) {
			name = n
			_, deadline = ctx.Deadline()
			return []byte(`{"schema":"incident-cli/investigate/v1"}`), nil, nil
		},
	}
	_, err := r.Investigate(context.Background(), Request{Service: "checkout", At: at, Window: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, "incident", name)
	assert.True(t, deadline, "every run has a deadline")
}

func TestRunCommand(t *testing.T) {
	out, _, err := runCommand(context.Background(), "echo", "hi")
	require.NoError(t, err)
	assert.Equal(t, "hi\n", string(out))
}
