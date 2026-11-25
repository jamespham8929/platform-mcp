// Package chaos reads the run records chaos-operator keeps in each
// ChaosExperiment's status.runs. The record format is set by chaos-operator's
// ADR 0004.
package chaos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"
)

const resource = "chaosexperiments.chaos.jamespham.io"

// Run is one chaos-operator run.
type Run struct {
	ID           string    `json:"id"`
	Experiment   string    `json:"experiment"`
	Namespace    string    `json:"namespace"`
	Trigger      string    `json:"trigger"`
	Type         string    `json:"type"`
	Service      string    `json:"service"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Outcome      string    `json:"outcome"`
	Injected     bool      `json:"injected"`
	PodsAffected int       `json:"pods_affected"`
}

// Lister returns a ChaosExperimentList as JSON.
type Lister interface {
	List(ctx context.Context) ([]byte, error)
}

// File reads a saved `kubectl get chaosexperiments -A -o json`.
type File struct{ Path string }

func (f File) List(context.Context) ([]byte, error) { return os.ReadFile(f.Path) }

// Kubectl asks the cluster through kubectl.
type Kubectl struct {
	Context string
	// Run is swapped in tests. Nil means exec.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (k Kubectl) List(ctx context.Context) ([]byte, error) {
	args := []string{"get", resource, "--all-namespaces", "--output", "json"}
	if k.Context != "" {
		args = append(args, "--context", k.Context)
	}
	run := k.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil && stderr.Len() > 0 {
				return nil, fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
			}
			return out, err
		}
	}
	return run(ctx, "kubectl", args...)
}

type list struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Status struct {
			Runs []record `json:"runs"`
		} `json:"status"`
	} `json:"items"`
}

type record struct {
	ID           string    `json:"id"`
	Trigger      string    `json:"trigger"`
	Type         string    `json:"type"`
	Service      string    `json:"service"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	Outcome      string    `json:"outcome"`
	Injected     *bool     `json:"injected"`
	PodsAffected int       `json:"podsAffected"`
}

var notInjected = map[string]bool{
	"no_targets": true, "skipped": true, "refused": true, "unsupported_for_safe_run": true,
}

// Parse returns every run in a ChaosExperimentList, newest first.
func Parse(b []byte) ([]Run, error) {
	var l list
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("parsing chaos experiments: %w", err)
	}
	var runs []Run
	for _, item := range l.Items {
		for _, r := range item.Status.Runs {
			injected := !notInjected[r.Outcome]
			if r.Injected != nil {
				injected = *r.Injected
			}
			runs = append(runs, Run{
				ID: r.ID, Experiment: item.Metadata.Name, Namespace: item.Metadata.Namespace,
				Trigger: r.Trigger, Type: r.Type, Service: r.Service,
				StartedAt: r.StartedAt.UTC(), FinishedAt: r.FinishedAt.UTC(),
				Outcome: r.Outcome, Injected: injected, PodsAffected: r.PodsAffected,
			})
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

// Filter keeps runs that started in [since, until], for the service when it
// is set, and only injected runs unless all is true.
func Filter(runs []Run, service string, since, until time.Time, all bool) []Run {
	out := []Run{}
	for _, r := range runs {
		if r.StartedAt.Before(since) || r.StartedAt.After(until) {
			continue
		}
		if service != "" && r.Service != service {
			continue
		}
		if !all && !r.Injected {
			continue
		}
		out = append(out, r)
	}
	return out
}
