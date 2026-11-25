package gym

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RecordingFormat versions the files a recording is made of.
const RecordingFormat = "platform-mcp/gym-recording/v1"

// The files in one recording directory.
const (
	FileScenario = "scenario.json"
	// FileEvents is the changes, in incident-cli's event file format.
	FileEvents = "events.json"
	// FileSeries is the shop's raw samples as gzipped OpenMetrics, which
	// promtool can load into a fresh Prometheus.
	FileSeries = "series.om.gz"
	// FileChaos is `kubectl get chaosexperiments -o json` for the shop's
	// namespace. It is the answer key, so an investigator never sees it
	// unless a replay asks for that on purpose.
	FileChaos = "chaos-experiments.json"
)

// Recording is what the gym saw during one scenario.
type Recording struct {
	Format     string     `json:"format"`
	Definition Definition `json:"definition"`
	Namespace  string     `json:"namespace"`
	// ShopStarted is when the shop's namespace was created. The recorded
	// samples start there.
	ShopStarted time.Time `json:"shop_started"`
	// FaultAt is when the first fault reached a pod, from chaos-operator's
	// run records. It is empty for a control scenario.
	FaultAt *time.Time `json:"fault_at,omitempty"`
	// DetectedAt is when the page fired. Replays set the clock here.
	DetectedAt time.Time `json:"detected_at"`
	Alert      string    `json:"alert"`
	SeriesFrom time.Time `json:"series_from"`
	SeriesTo   time.Time `json:"series_to"`
	Series     int       `json:"series"`
	Samples    int       `json:"samples"`
	Truth      Truth     `json:"truth"`
}

// Truth is the resolved answer key.
type Truth struct {
	Service string `json:"service"`
	// Accept holds the rules, with change names turned into evidence IDs.
	Accept []Rule `json:"accept"`
	// Distractors are the changes that did not cause the incident.
	Distractors []string `json:"distractors,omitempty"`
	// ChaosRuns are the IDs of the runs that injected the fault before the
	// page. Citing one counts as naming the cause.
	ChaosRuns []string `json:"chaos_runs,omitempty"`
}

// Event is one change, in incident-cli's event file format.
type Event struct {
	ID        string            `json:"id"`
	Source    string            `json:"source"`
	Service   string            `json:"service"`
	Title     string            `json:"title"`
	Timestamp time.Time         `json:"timestamp"`
	Magnitude float64           `json:"magnitude"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Loaded is a recording and the directory it came from.
type Loaded struct {
	Recording
	Dir string
}

func (l Loaded) Path(file string) string { return filepath.Join(l.Dir, file) }

// LoadRecordings reads every recording under dir, one per subdirectory.
func LoadRecordings(dir string) ([]Loaded, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Loaded
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(filepath.Join(sub, FileScenario))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var r Recording
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", sub, err)
		}
		if r.Format != RecordingFormat {
			return nil, fmt.Errorf("%s has format %q, this build reads %q", sub, r.Format, RecordingFormat)
		}
		for _, f := range []string{FileEvents, FileSeries, FileChaos} {
			if _, err := os.Stat(filepath.Join(sub, f)); err != nil {
				return nil, fmt.Errorf("recording %s is incomplete: %w", e.Name(), err)
			}
		}
		out = append(out, Loaded{Recording: r, Dir: sub})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no recordings in %s", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Definition.Name < out[j].Definition.Name })
	return out, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
