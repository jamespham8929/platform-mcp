// Package promapi runs instant PromQL queries with a time limit and a cap on
// how much comes back. An agent can ask for a lot. A query that times out or
// returns too much is an error the agent can see, not a stuck server.
package promapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sample is one series in an instant vector.
type Sample struct {
	Labels map[string]string
	Value  float64
	Time   time.Time
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Timeout bounds each query, on the client and on the Prometheus side.
	Timeout time.Duration
	// MaxSeries refuses a result with more series than this.
	MaxSeries int
	// MaxBytes caps the response body.
	MaxBytes int64
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		HTTP:      http.DefaultClient,
		Timeout:   10 * time.Second,
		MaxSeries: 500,
		MaxBytes:  8 << 20,
	}
}

// ErrTooManySeries is returned when a result passes MaxSeries.
var ErrTooManySeries = errors.New("query returned too many series")

type response struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query runs an instant query at the given time. Only vector results are
// supported, which is what every caller here needs.
func (c *Client) Query(ctx context.Context, query string, at time.Time) ([]Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	form := url.Values{}
	form.Set("query", query)
	form.Set("time", strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64))
	form.Set("timeout", c.Timeout.String())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/query",
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying prometheus: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading prometheus response: %w", err)
	}
	if int64(len(body)) > c.MaxBytes {
		return nil, fmt.Errorf("prometheus response is larger than %d bytes", c.MaxBytes)
	}

	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("prometheus returned %s with a body that is not JSON", resp.Status)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus %s: %s", r.ErrorType, r.Error)
	}
	if r.Data.ResultType != "vector" {
		return nil, fmt.Errorf("expected a vector, prometheus returned %s", r.Data.ResultType)
	}
	if len(r.Data.Result) > c.MaxSeries {
		return nil, fmt.Errorf("%w: %d, the limit is %d", ErrTooManySeries, len(r.Data.Result), c.MaxSeries)
	}

	out := make([]Sample, 0, len(r.Data.Result))
	for _, s := range r.Data.Result {
		ts, ok := s.Value[0].(float64)
		raw, ok2 := s.Value[1].(string)
		if !ok || !ok2 {
			return nil, errors.New("prometheus returned a sample in an unknown shape")
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing sample value %q: %w", raw, err)
		}
		out = append(out, Sample{
			Labels: s.Metric,
			Value:  v,
			Time:   time.UnixMilli(int64(ts * 1000)).UTC(),
		})
	}
	return out, nil
}
