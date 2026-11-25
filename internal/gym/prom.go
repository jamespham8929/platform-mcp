package gym

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prom reads raw samples and alerts from the gym's Prometheus.
type Prom struct {
	BaseURL string
	HTTP    *http.Client
}

// Series is one time series with its raw samples. Labels include __name__.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// Sample keeps the timestamp and value exactly as Prometheus wrote them, so
// the recording does not round anything.
type Sample struct {
	Time  json.Number
	Value string
}

type apiResponse struct {
	Status    string          `json:"status"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	Data      json.RawMessage `json:"data"`
}

func (p Prom) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

// call sends a request to the API. Queries go as a POST form, because a raw
// selector can be long. /api/v1/alerts only answers GET.
func (p Prom) call(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	target := strings.TrimRight(p.BaseURL, "/") + path
	var body io.Reader
	if method == http.MethodGet {
		if len(form) > 0 {
			target += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	var parsed apiResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("%s returned %s", path, resp.Status)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("%s failed (%s): %s", path, parsed.ErrorType, parsed.Error)
	}
	return parsed.Data, nil
}

// Raw returns every raw sample of the series that match selector, from from
// to to. It asks for a range vector in an instant query, which is how the
// Prometheus API hands out raw samples instead of evaluated steps.
func (p Prom) Raw(ctx context.Context, selector string, from, to time.Time) ([]Series, error) {
	seconds := int64(to.Sub(from).Seconds())
	if seconds < 1 {
		return nil, fmt.Errorf("empty range %s to %s", from, to)
	}
	data, err := p.call(ctx, http.MethodPost, "/api/v1/query", url.Values{
		"query": {fmt.Sprintf("%s[%ds]", selector, seconds)},
		"time":  {strconv.FormatInt(to.Unix(), 10)},
	})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, err
	}
	if parsed.ResultType != "matrix" {
		return nil, fmt.Errorf("raw query returned %q, want a matrix", parsed.ResultType)
	}
	out := make([]Series, 0, len(parsed.Result))
	for _, r := range parsed.Result {
		s := Series{Labels: r.Metric, Samples: make([]Sample, 0, len(r.Values))}
		for _, v := range r.Values {
			ts, ok1 := v[0].(json.Number)
			val, ok2 := v[1].(string)
			if !ok1 || !ok2 {
				return nil, fmt.Errorf("unexpected sample %v", v)
			}
			s.Samples = append(s.Samples, Sample{Time: ts, Value: val})
		}
		out = append(out, s)
	}
	return out, nil
}

// Alert is one alert from /api/v1/alerts.
type Alert struct {
	Name     string
	State    string
	ActiveAt time.Time
	Labels   map[string]string
}

// Alerts returns the pending and firing alerts.
func (p Prom) Alerts(ctx context.Context) ([]Alert, error) {
	data, err := p.call(ctx, http.MethodGet, "/api/v1/alerts", nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Alerts []struct {
			Labels   map[string]string `json:"labels"`
			State    string            `json:"state"`
			ActiveAt time.Time         `json:"activeAt"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(parsed.Alerts))
	for _, a := range parsed.Alerts {
		out = append(out, Alert{Name: a.Labels["alertname"], State: a.State, ActiveAt: a.ActiveAt, Labels: a.Labels})
	}
	return out, nil
}
