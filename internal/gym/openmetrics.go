package gym

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteOpenMetrics writes series in the OpenMetrics text format that
// `promtool tsdb create-blocks-from openmetrics` reads. It writes no TYPE
// lines, so every family is "unknown" and keeps its name as it is. The
// samples of one family must be next to each other, and each series must be
// in time order, so both are sorted here.
func WriteOpenMetrics(w io.Writer, series []Series) (samples int, err error) {
	sorted := make([]Series, 0, len(series))
	for _, s := range series {
		if s.Labels["__name__"] == "" {
			return 0, fmt.Errorf("series %v has no metric name", s.Labels)
		}
		sorted = append(sorted, s)
	}
	keys := make([]string, len(sorted))
	for i, s := range sorted {
		keys[i] = s.Labels["__name__"] + labelString(s.Labels)
	}
	idx := make([]int, len(sorted))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return keys[idx[a]] < keys[idx[b]] })

	bw := bufio.NewWriter(w)
	for _, i := range idx {
		s := sorted[i]
		prefix := s.Labels["__name__"] + labelString(s.Labels) + " "
		// The API returns samples in time order. Check rather than trust.
		last := -1.0
		for _, smp := range s.Samples {
			t, err := smp.Time.Float64()
			if err != nil {
				return 0, fmt.Errorf("series %s: bad timestamp %q", prefix, smp.Time)
			}
			if t <= last {
				return 0, fmt.Errorf("series %s is not in time order", prefix)
			}
			last = t
		}
		for _, smp := range s.Samples {
			if _, err := fmt.Fprintf(bw, "%s%s %s\n", prefix, smp.Value, smp.Time); err != nil {
				return 0, err
			}
			samples++
		}
	}
	if _, err := bw.WriteString("# EOF\n"); err != nil {
		return 0, err
	}
	return samples, bw.Flush()
}

// labelString renders {a="1",b="2"} with sorted names, without __name__.
func labelString(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for k := range labels {
		if k != "__name__" {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(labels[k]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}
