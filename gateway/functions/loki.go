package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type LokiClient struct {
	baseURL string
	hc      *http.Client
}

func NewLokiClient(baseURL string) *LokiClient {
	return &LokiClient{
		baseURL: baseURL,
		hc:      &http.Client{Timeout: 5 * time.Second},
	}
}

type LogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}

type lokiQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// FunctionLogs returns recent log lines emitted by a function's VMs,
// newest first. since is a Loki duration (e.g. "1h", "30m").
func (l *LokiClient) FunctionLogs(ctx context.Context, functionID, since string, limit int) ([]LogLine, error) {
	if since == "" {
		since = "1h"
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	query := fmt.Sprintf(`{function_id=%q}`, functionID)
	params := url.Values{}
	params.Set("query", query)
	params.Set("since", since)
	params.Set("limit", fmt.Sprintf("%d", limit))
	params.Set("direction", "backward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		l.baseURL+"/loki/api/v1/query_range?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := l.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki query failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("loki returned status %d", resp.StatusCode)
	}

	var parsed lokiQueryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("invalid loki response: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("loki query status: %s", parsed.Status)
	}

	var lines []LogLine
	for _, stream := range parsed.Data.Result {
		for _, v := range stream.Values {
			tsNs, _ := strconv.ParseInt(v[0], 10, 64)
			lines = append(lines, LogLine{
				Timestamp: time.Unix(0, tsNs),
				Line:      v[1],
			})
		}
	}
	return lines, nil
}
