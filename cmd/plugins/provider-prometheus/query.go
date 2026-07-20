/*
Copyright 2026 The Parallax Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// maxBody bounds how much of a query response we read, guarding against a runaway
// backend returning an unbounded body.
const maxBody = 8 << 20 // 8 MiB

// promResponse is the envelope returned by the Prometheus HTTP query API
// (GET /api/v1/query). result is left raw because its shape depends on resultType.
type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// windowRange derives the PromQL range-vector duration (e.g. "300s") from the
// recorded measurement window and returns the instant (t2) to evaluate the query at.
func windowRange(win *pluginv1.Window) (string, time.Time, error) {
	t1, err := time.Parse(time.RFC3339, win.GetT1Rfc3339())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse window t1 %q: %w", win.GetT1Rfc3339(), err)
	}
	t2, err := time.Parse(time.RFC3339, win.GetT2Rfc3339())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse window t2 %q: %w", win.GetT2Rfc3339(), err)
	}
	secs := int64(t2.Sub(t1).Seconds())
	if secs <= 0 {
		return "", time.Time{}, fmt.Errorf("non-positive measurement window [%s, %s]", win.GetT1Rfc3339(), win.GetT2Rfc3339())
	}
	return strconv.FormatInt(secs, 10) + "s", t2, nil
}

// query performs a single instant query and reduces the result to one float64.
// It never returns an error: a failed query yields (0, false, detail) so the caller
// can record a not-ok SLIValue with evidence rather than aborting the whole Collect.
func (s *server) query(ctx context.Context, endpoint, promQL string, evalTime time.Time) (value float64, ok bool, detail string) {
	u := endpoint + "/api/v1/query"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false, fmt.Sprintf("build request: %v", err)
	}
	params := url.Values{}
	params.Set("query", promQL)
	params.Set("time", strconv.FormatInt(evalTime.Unix(), 10))
	req.URL.RawQuery = params.Encode()

	httpResp, err := s.http.Do(req)
	if err != nil {
		return 0, false, fmt.Sprintf("prometheus query: %v", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBody))
	if err != nil {
		return 0, false, fmt.Sprintf("read prometheus response: %v", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return 0, false, fmt.Sprintf("prometheus returned %s: %s", httpResp.Status, strings.TrimSpace(string(body)))
	}

	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return 0, false, fmt.Sprintf("decode prometheus response: %v", err)
	}
	if pr.Status != "success" {
		return 0, false, fmt.Sprintf("prometheus status %q (%s): %s", pr.Status, pr.ErrorType, pr.Error)
	}

	v, err := reduceScalar(pr.Data.ResultType, pr.Data.Result)
	if err != nil {
		return 0, false, err.Error()
	}
	return v, true, ""
}

// reduceScalar collapses a Prometheus instant-query result to a single float64.
// Scalar and single-series vector results are the common case for aggregated SLI
// queries (§11); a multi-series result is reduced to its first series.
func reduceScalar(resultType string, raw json.RawMessage) (float64, error) {
	switch resultType {
	case "scalar":
		// [ <ts>, "<value>" ]
		var sample [2]json.RawMessage
		if err := json.Unmarshal(raw, &sample); err != nil {
			return 0, fmt.Errorf("decode scalar result: %w", err)
		}
		return sampleValue(sample[1])

	case "vector":
		// [ { "metric": {...}, "value": [ <ts>, "<value>" ] }, ... ]
		var samples []struct {
			Value [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(raw, &samples); err != nil {
			return 0, fmt.Errorf("decode vector result: %w", err)
		}
		if len(samples) == 0 {
			return 0, fmt.Errorf("empty vector result")
		}
		return sampleValue(samples[0].Value[1])

	case "matrix":
		// [ { "metric": {...}, "values": [ [ <ts>, "<value>" ], ... ] }, ... ]
		// TODO(m1): a range result should be reduced with an explicit aggregation;
		// for now we take the last sample of the first series.
		var series []struct {
			Values [][2]json.RawMessage `json:"values"`
		}
		if err := json.Unmarshal(raw, &series); err != nil {
			return 0, fmt.Errorf("decode matrix result: %w", err)
		}
		if len(series) == 0 || len(series[0].Values) == 0 {
			return 0, fmt.Errorf("empty matrix result")
		}
		last := series[0].Values[len(series[0].Values)-1]
		return sampleValue(last[1])

	default:
		return 0, fmt.Errorf("unsupported prometheus resultType %q", resultType)
	}
}

// sampleValue decodes a Prometheus sample value, which is JSON-encoded as a string
// and may carry the special tokens NaN / +Inf / -Inf.
func sampleValue(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("decode sample value: %w", err)
	}
	switch s {
	case "NaN":
		return math.NaN(), nil
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parse sample value %q: %w", s, err)
	}
	return f, nil
}
