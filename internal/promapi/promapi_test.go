package promapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func serve(t *testing.T, body string, check func(*http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL + "/")
}

const vector = `{"status":"success","data":{"resultType":"vector","result":[
	{"metric":{"__name__":"up","job":"a"},"value":[1768478280.5,"1"]},
	{"metric":{"__name__":"up","job":"b"},"value":[1768478280.5,"NaN"]}]}}`

func TestQuery(t *testing.T) {
	at := time.Date(2026, 1, 15, 11, 58, 0, 0, time.UTC)
	c := serve(t, vector, func(r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/query", r.URL.Path)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "up", r.Form.Get("query"))
		assert.Equal(t, "1768478280.000", r.Form.Get("time"))
		assert.Equal(t, "10s", r.Form.Get("timeout"), "Prometheus gets the same limit")
	})
	samples, err := c.Query(context.Background(), "up", at)
	require.NoError(t, err)
	require.Len(t, samples, 2)
	assert.Equal(t, "a", samples[0].Labels["job"])
	assert.Equal(t, 1.0, samples[0].Value)
	assert.Equal(t, time.UnixMilli(1768478280500).UTC(), samples[0].Time)
}

func TestQueryLimitsAndErrors(t *testing.T) {
	c := serve(t, vector, nil)
	c.MaxSeries = 1
	_, err := c.Query(context.Background(), "up", time.Now())
	assert.True(t, errors.Is(err, ErrTooManySeries))

	c = serve(t, vector, nil)
	c.MaxBytes = 10
	_, err = c.Query(context.Background(), "up", time.Now())
	assert.ErrorContains(t, err, "larger than")

	cases := map[string]string{
		`{"status":"error","errorType":"bad_data","error":"parse error"}`:                            "parse error",
		`{"status":"success","data":{"resultType":"matrix","result":[]}}`:                            "expected a vector",
		`<html>gateway timeout</html>`:                                                               "not JSON",
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,2]}]}}`: "unknown shape",
	}
	for body, want := range cases {
		_, err := serve(t, body, nil).Query(context.Background(), "up", time.Now())
		assert.ErrorContains(t, err, want, body)
	}
}

func TestQueryTimesOut(t *testing.T) {
	c := serve(t, vector, func(*http.Request) { time.Sleep(200 * time.Millisecond) })
	c.Timeout = 20 * time.Millisecond
	_, err := c.Query(context.Background(), "up", time.Now())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "Timeout"), err.Error())
}
