package gym

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jamespham8929/platform-mcp/internal/chaos"
)

// Runner runs a command such as kubectl or helm.
type Runner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// ExecRunner runs real commands.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Recorder runs scenarios on a gym cluster that gym/up.sh set up.
type Recorder struct {
	Kube        Runner
	Prom        Prom
	KubeContext string
	// Chart is gitops-platform-demo's sample-service chart, for deploys.
	Chart string
	// Out is the recordings directory. Each scenario gets a subdirectory.
	Out string
	// Warmup is how long a shop must run before its scenario starts.
	Warmup time.Duration
	// PageTimeout is how long to wait for the page after the fault.
	PageTimeout time.Duration
	// Settle is how long to keep recording after the page.
	Settle time.Duration
	// RunTimeout is how long to wait for chaos-operator's run record after
	// the page. A run is recorded when it ends, so a long latency fault is
	// recorded minutes after it started.
	RunTimeout time.Duration
	Now        func() time.Time
	Sleep      func(ctx context.Context, d time.Duration) error
	Log        *slog.Logger
	// ID makes the random part of event IDs. Tests fix it.
	ID func() string
}

func (r *Recorder) defaults() {
	if r.Kube == nil {
		r.Kube = ExecRunner{}
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Sleep == nil {
		r.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
	if r.ID == nil {
		r.ID = func() string { return fmt.Sprintf("%06x", rand.Uint32()&0xffffff) }
	}
	if r.PageTimeout == 0 {
		r.PageTimeout = 10 * time.Minute
	}
	if r.RunTimeout == 0 {
		r.RunTimeout = 15 * time.Minute
	}
}

// RecordAll runs the scenarios at the same time, each in its own shop, and
// starts them stagger apart so their logs are easy to follow. It returns
// every error, and keeps the recordings that worked.
func (r *Recorder) RecordAll(ctx context.Context, defs []Definition, stagger time.Duration) error {
	r.defaults()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i, d := range defs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Sleep(ctx, time.Duration(i)*stagger); err != nil {
				return
			}
			if _, err := r.Record(ctx, d); err != nil {
				r.Log.Error("scenario failed", "scenario", d.Name, "error", err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// seriesSelector picks what a recording keeps: the shop's request and
// upstream metrics and its replica counts. Probe and info routes are left
// out, because nothing reads them and they would double the size.
func seriesSelector(ns string) string {
	return fmt.Sprintf(`{namespace=%q, __name__=~"%s", route!~"/health|/ready|/api/v1/info"}`, ns,
		"http_requests_total|http_request_duration_seconds_(bucket|sum|count)|"+
			"upstream_requests_total|upstream_request_duration_seconds_(bucket|sum|count)|"+
			"kube_deployment_status_replicas_available|kube_deployment_spec_replicas")
}

const pageLabel = "gym_namespace"

// Record runs one scenario and writes its recording.
func (r *Recorder) Record(ctx context.Context, d Definition) (*Recording, error) {
	r.defaults()
	ns := d.Namespace()
	log := r.Log.With("scenario", d.Name)

	created, err := r.namespaceCreated(ctx, ns)
	if err != nil {
		return nil, err
	}
	if wait := created.Add(r.Warmup).Sub(r.Now()); wait > 0 {
		log.Info("waiting for the shop to warm up", "for", wait.Round(time.Second))
		if err := r.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	if page, _, err := r.firingPage(ctx, ns); err != nil {
		return nil, err
	} else if page {
		return nil, fmt.Errorf("%s is already paging before the scenario started", ns)
	}

	// The fault goes in at zero. Changes before it go in first.
	earliest, _ := d.offsets()
	zero := r.Now().Add(-earliest + 5*time.Second)
	type action struct {
		at     time.Time
		change *Change
	}
	var actions []action
	if d.Fault != nil {
		actions = append(actions, action{at: zero})
	}
	for i := range d.Changes {
		c := &d.Changes[i]
		actions = append(actions, action{at: zero.Add(time.Duration(c.AtSeconds) * time.Second), change: c})
	}
	sort.SliceStable(actions, func(i, j int) bool { return actions[i].at.Before(actions[j].at) })

	var events []Event
	changeIDs := map[string]string{}
	for _, a := range actions {
		if wait := a.at.Sub(r.Now()); wait > 0 {
			if err := r.Sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		if a.change == nil {
			log.Info("injecting the fault", "type", d.Fault.Type, "service", d.Fault.Service)
			if err := r.applyFault(ctx, d); err != nil {
				return nil, err
			}
			continue
		}
		ev, err := r.applyChange(ctx, ns, *a.change)
		if err != nil {
			return nil, err
		}
		log.Info("made a change", "kind", a.change.Kind, "service", a.change.Service, "id", ev.ID)
		events = append(events, ev)
		changeIDs[a.change.Name] = "event:" + ev.ID
	}

	detected, alert, err := r.waitForPage(ctx, ns)
	if err != nil {
		return nil, err
	}
	log.Info("paged", "alert", alert, "at", detected.Format(time.RFC3339))
	if err := r.Sleep(ctx, r.Settle); err != nil {
		return nil, err
	}
	to := r.Now().Truncate(time.Second)

	series, err := r.Prom.Raw(ctx, seriesSelector(ns), created, to)
	if err != nil {
		return nil, fmt.Errorf("reading samples: %w", err)
	}
	runs, chaosJSON, err := r.waitForRuns(ctx, d, detected)
	if err != nil {
		return nil, err
	}

	rec := &Recording{
		Format: RecordingFormat, Definition: d, Namespace: ns, ShopStarted: created,
		DetectedAt: detected, Alert: alert, SeriesFrom: created, SeriesTo: to, Series: len(series),
		Truth: resolveTruth(d, changeIDs),
	}
	// The run records say when the fault really reached a pod. A scheduled
	// fault starts at the next minute, not when the experiment is created.
	for _, run := range runs {
		if run.Injected && !run.StartedAt.After(detected) {
			rec.Truth.ChaosRuns = append(rec.Truth.ChaosRuns, run.ID)
			if rec.FaultAt == nil || run.StartedAt.Before(*rec.FaultAt) {
				t := run.StartedAt
				rec.FaultAt = &t
			}
		}
	}
	sort.Strings(rec.Truth.ChaosRuns)
	if d.Fault != nil && rec.FaultAt == nil {
		return nil, fmt.Errorf("the page fired, but no chaos run injected anything before it")
	}

	dir := filepath.Join(r.Out, d.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var om bytes.Buffer
	zw := gzip.NewWriter(&om)
	if rec.Samples, err = WriteOpenMetrics(zw, series); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, FileSeries), om.Bytes(), 0o644); err != nil {
		return nil, err
	}
	if events == nil {
		events = []Event{}
	}
	if err := writeJSON(filepath.Join(dir, FileEvents), events); err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, chaosJSON, "", "  "); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, FileChaos), append(pretty.Bytes(), '\n'), 0o644); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, FileScenario), rec); err != nil {
		return nil, err
	}
	log.Info("recorded", "series", rec.Series, "samples", rec.Samples, "dir", dir)

	// Stop a repeating fault. A latency fault ends on its own schedule.
	if d.Fault != nil {
		if _, err := r.Kube.Run(ctx, nil, "kubectl", "--context", r.KubeContext, "-n", ns,
			"delete", "chaosexperiments.chaos.jamespham.io", "fault", "--wait=false"); err != nil {
			log.Warn("could not delete the experiment", "error", err)
		}
	}
	return rec, nil
}

func resolveTruth(d Definition, changeIDs map[string]string) Truth {
	t := Truth{Service: d.Truth.Service}
	cause := map[string]bool{}
	for _, rule := range d.Truth.Accept {
		if rule.Change != "" {
			rule.EvidenceID = changeIDs[rule.Change]
			cause[rule.Change] = true
		}
		t.Accept = append(t.Accept, rule)
	}
	for _, c := range d.Changes {
		if !cause[c.Name] {
			t.Distractors = append(t.Distractors, changeIDs[c.Name])
		}
	}
	return t
}

func (r *Recorder) namespaceCreated(ctx context.Context, ns string) (time.Time, error) {
	out, err := r.Kube.Run(ctx, nil, "kubectl", "--context", r.KubeContext,
		"get", "namespace", ns, "-o", "jsonpath={.metadata.creationTimestamp}")
	if err != nil {
		return time.Time{}, fmt.Errorf("the shop for this scenario is missing, run gym/up.sh: %w", err)
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
}

func (r *Recorder) applyFault(ctx context.Context, d Definition) error {
	f := d.Fault
	spec := map[string]any{
		"type":     f.Type,
		"service":  f.Service,
		"selector": map[string]any{"matchLabels": map[string]string{"app.kubernetes.io/name": f.Service}},
	}
	if f.Type != FaultPodFailure {
		// tc and stress-ng live in the sidecar. The app image has neither.
		spec["container"] = "chaos-agent"
	}
	if f.Schedule != "" {
		spec["schedule"] = f.Schedule
	}
	for k, v := range f.Spec {
		spec[k] = v
	}
	obj := map[string]any{
		"apiVersion": "chaos.jamespham.io/v1alpha1",
		"kind":       "ChaosExperiment",
		"metadata":   map[string]any{"name": "fault", "namespace": d.Namespace()},
		"spec":       spec,
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = r.Kube.Run(ctx, b, "kubectl", "--context", r.KubeContext, "apply", "-f", "-")
	return err
}

func (r *Recorder) applyChange(ctx context.Context, ns string, c Change) (Event, error) {
	id := r.ID()
	ev := Event{
		Service: c.Service, Title: c.Title, Timestamp: r.Now().UTC().Truncate(time.Second),
		// Every change gets the same magnitude, so magnitude says nothing
		// about which change is the cause.
		Magnitude: 0.6,
	}
	ev.Source = c.Kind
	switch c.Flag {
	case "":
		prefix := "deploy-"
		if c.Kind == KindConfigChange {
			prefix = "config-"
		}
		ev.ID = prefix + c.Service + "-" + id
		ev.Metadata = map[string]string{"release": id}
		// A new release always rolls the pods, the way a real deploy or
		// config rollout does.
		args := []string{"upgrade", c.Service, r.Chart, "--kube-context", r.KubeContext, "--namespace", ns,
			"--reuse-values", "--set", "podAnnotations.gym-release=" + id}
		args = append(args, helmValues("--set", c.Set)...)
		args = append(args, helmValues("--set-json", c.SetJSON)...)
		if _, err := r.Kube.Run(ctx, nil, "helm", args...); err != nil {
			return ev, err
		}
	default:
		ev.ID = "flag-" + c.Service + "-" + id
		ev.Metadata = map[string]string{"flag": c.Flag}
		patch, _ := json.Marshal(map[string]any{"data": map[string]string{c.Flag: "on"}})
		if _, err := r.Kube.Run(ctx, nil, "kubectl", "--context", r.KubeContext, "-n", ns,
			"patch", "configmap", "flags", "--type", "merge", "-p", string(patch)); err != nil {
			return ev, err
		}
	}
	return ev, nil
}

// helmValues turns a map into flag pairs, in a fixed order.
func helmValues(flag string, values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, flag, k+"="+values[k])
	}
	return out
}

func (r *Recorder) firingPage(ctx context.Context, ns string) (bool, string, error) {
	alerts, err := r.Prom.Alerts(ctx)
	if err != nil {
		return false, "", err
	}
	for _, a := range alerts {
		if a.Labels[pageLabel] == ns && a.State == "firing" {
			return true, a.Name, nil
		}
	}
	return false, "", nil
}

func (r *Recorder) waitForPage(ctx context.Context, ns string) (time.Time, string, error) {
	deadline := r.Now().Add(r.PageTimeout)
	for {
		firing, name, err := r.firingPage(ctx, ns)
		if err != nil {
			r.Log.Warn("reading alerts failed, trying again", "error", err)
		} else if firing {
			return r.Now().UTC().Truncate(time.Second), name, nil
		}
		if r.Now().After(deadline) {
			return time.Time{}, "", fmt.Errorf("no page within %s", r.PageTimeout)
		}
		if err := r.Sleep(ctx, 5*time.Second); err != nil {
			return time.Time{}, "", err
		}
	}
}

// waitForRuns returns the experiment list once it holds a run that injected
// the fault before the page. A control scenario has no fault, so it returns
// at once. A run that failed to inject means the scenario did not happen as
// planned, and that is an error.
func (r *Recorder) waitForRuns(ctx context.Context, d Definition, detected time.Time) ([]chaos.Run, []byte, error) {
	deadline := r.Now().Add(r.RunTimeout)
	for {
		raw, err := r.Kube.Run(ctx, nil, "kubectl", "--context", r.KubeContext, "-n", d.Namespace(),
			"get", "chaosexperiments.chaos.jamespham.io", "-o", "json")
		if err != nil {
			return nil, nil, err
		}
		runs, err := chaos.Parse(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("reading chaos runs: %w", err)
		}
		if d.Fault == nil {
			return runs, raw, nil
		}
		for _, run := range runs {
			if run.StartedAt.After(detected) {
				continue
			}
			if run.Injected {
				return runs, raw, nil
			}
			return nil, nil, fmt.Errorf("chaos run %s did not inject the fault: %s", run.ID, run.Outcome)
		}
		if r.Now().After(deadline) {
			return nil, nil, fmt.Errorf("no chaos run record within %s of the page", r.RunTimeout)
		}
		r.Log.Info("waiting for chaos-operator to record the run", "scenario", d.Name)
		if err := r.Sleep(ctx, 15*time.Second); err != nil {
			return nil, nil, err
		}
	}
}
