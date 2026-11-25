// Package gym records incidents with a known cause on a kind cluster, and
// replays them later without the cluster.
//
// A scenario breaks one shop in a known way. chaos-operator injects the
// fault, and the gym makes the distracting changes a real incident has, such
// as an unrelated deploy. The page comes from Prometheus, the same way a real
// one would. Then the gym records what an investigator could have seen: the
// changes as incident-cli events, and the shop's raw metric samples.
//
// A replay loads the samples into a local Prometheus, runs incident-cli and
// the MCP tools on top, and grades the investigator. It needs no cluster, so
// CI can replay every recording in seconds.
package gym

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Definition is one scenario file in gym/scenarios.
type Definition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// PagedService is the service whose page starts the investigation.
	PagedService string   `json:"paged_service"`
	Fault        *Fault   `json:"fault,omitempty"`
	Changes      []Change `json:"changes,omitempty"`
	Truth        TruthDef `json:"truth"`
	// Group says how the scenario is used. "tuning" scenarios shaped
	// incident-cli's rules. "held-out" scenarios were written and pushed
	// before anyone saw how an investigator does on them.
	Group string `json:"group,omitempty"`
}

// Fault is the chaos experiment that causes the incident. A control scenario
// has none, and one of its changes is the cause instead.
type Fault struct {
	Type    string `json:"type"`
	Service string `json:"service"`
	// Schedule repeats the fault on a cron schedule, like a crash loop. Empty
	// runs it once, when the experiment is created.
	Schedule string `json:"schedule,omitempty"`
	// Spec is merged into the ChaosExperiment spec, for example
	// {"networkLatency": {"latencyMs": 400}}.
	Spec map[string]any `json:"spec,omitempty"`
}

// Change is something the gym does to the shop that an incident tool would
// record as an event, such as a deploy or a flag flip.
type Change struct {
	Name string `json:"name"`
	// AtSeconds is the time of the change relative to the fault, so -180 is
	// three minutes before it.
	AtSeconds int    `json:"at_seconds"`
	Kind      string `json:"kind"`
	Service   string `json:"service"`
	Title     string `json:"title"`
	// Flag is the flag a feature_flag or config_change turns on, in the
	// shop's flags ConfigMap. It changes nothing the service reads.
	Flag string `json:"flag,omitempty"`
	// Set and SetJSON are Helm values for a deploy or a config_change, such
	// as failureRate, or env with a new UPSTREAM_TIMEOUT. The service reads
	// these, so this kind of change can break it.
	Set     map[string]string `json:"set,omitempty"`
	SetJSON map[string]string `json:"set_json,omitempty"`
}

// TruthDef says what counts as naming the cause.
type TruthDef struct {
	Service string `json:"service"`
	Accept  []Rule `json:"accept"`
}

// Rule matches the evidence an answer cites as its cause. Every field that is
// set must match. Change names a change in the same scenario. The recording
// replaces it with that change's evidence ID.
type Rule struct {
	Source     string `json:"source,omitempty"`
	Service    string `json:"service,omitempty"`
	Query      string `json:"query,omitempty"`
	Change     string `json:"change,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
}

const (
	KindDeploy       = "deploy"
	KindFeatureFlag  = "feature_flag"
	KindConfigChange = "config_change"

	GroupTuning  = "tuning"
	GroupHeldOut = "held-out"

	FaultNetworkLatency = "network-latency"
	FaultPodFailure     = "pod-failure"
	FaultCPUStress      = "cpu-stress"
)

// The shop every scenario runs on. gym/up.sh deploys it.
var shopServices = map[string]bool{"frontend": true, "checkout": true, "inventory": true, "recommendations": true}

// A namespace name is "gym-" plus the scenario name, so the name must fit.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,50}[a-z0-9])?$`)

// Namespace is where the scenario's shop runs.
func (d Definition) Namespace() string { return "gym-" + d.Name }

// Validate catches a broken scenario file before a two hour run does.
func (d Definition) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !namePattern.MatchString(d.Name) {
		bad("name %q must be lowercase letters, digits and dashes", d.Name)
	}
	if !shopServices[d.PagedService] {
		bad("paged_service %q is not a shop service", d.PagedService)
	}
	if d.Fault != nil {
		switch d.Fault.Type {
		case FaultNetworkLatency, FaultPodFailure, FaultCPUStress:
		default:
			bad("fault type %q is not one chaos-operator runs", d.Fault.Type)
		}
		if !shopServices[d.Fault.Service] {
			bad("fault service %q is not a shop service", d.Fault.Service)
		}
	}
	names := map[string]bool{}
	for _, c := range d.Changes {
		if c.Name == "" || names[c.Name] {
			bad("change name %q is empty or used twice", c.Name)
		}
		names[c.Name] = true
		if !shopServices[c.Service] {
			bad("change %s: service %q is not a shop service", c.Name, c.Service)
		}
		if c.Title == "" {
			bad("change %s has no title", c.Name)
		}
		helm := len(c.Set)+len(c.SetJSON) > 0
		switch c.Kind {
		case KindDeploy:
			if c.Flag != "" {
				bad("change %s: a deploy has no flag", c.Name)
			}
		case KindFeatureFlag:
			if c.Flag == "" || helm {
				bad("change %s: a feature_flag needs a flag and no Helm values", c.Name)
			}
		case KindConfigChange:
			if (c.Flag == "") == !helm {
				bad("change %s: a config_change needs a flag or Helm values, not both", c.Name)
			}
		default:
			bad("change %s: kind %q is not deploy, feature_flag or config_change", c.Name, c.Kind)
		}
	}
	if d.Fault == nil && len(d.Changes) == 0 {
		bad("a scenario needs a fault or a change")
	}
	switch d.Group {
	case "", GroupTuning, GroupHeldOut:
	default:
		bad("group %q is not tuning or held-out", d.Group)
	}
	if !shopServices[d.Truth.Service] {
		bad("truth service %q is not a shop service", d.Truth.Service)
	}
	if len(d.Truth.Accept) == 0 {
		bad("truth needs at least one accept rule")
	}
	for _, r := range d.Truth.Accept {
		if r.Change != "" && !names[r.Change] {
			bad("accept rule names change %q, which does not exist", r.Change)
		}
		if r.Source == "" && r.Service == "" && r.Query == "" && r.Change == "" && r.EvidenceID == "" {
			bad("an accept rule must say something")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("scenario %s: %w", d.Name, errors.Join(errs...))
	}
	return nil
}

// LoadDefinitions reads and checks every *.json file in dir.
func LoadDefinitions(dir string) ([]Definition, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no scenarios in %s", dir)
	}
	sort.Strings(paths)
	defs := make([]Definition, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var d Definition
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", p, err)
		}
		if want := filepath.Base(p); want != d.Name+".json" {
			return nil, fmt.Errorf("%s holds scenario %q, the file name must match", p, d.Name)
		}
		if err := d.Validate(); err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	return defs, nil
}

// offsets returns the earliest and latest change times relative to the fault.
func (d Definition) offsets() (earliest, latest time.Duration) {
	for _, c := range d.Changes {
		at := time.Duration(c.AtSeconds) * time.Second
		earliest, latest = min(earliest, at), max(latest, at)
	}
	return earliest, latest
}
