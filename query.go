package main

import (
	"bytes"
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

	"github.com/redis/go-redis/v9"
)

type queryScope struct {
	Profile, Namespace, Cluster, Route string
	Observed                           time.Time
}

func (s *service) queryObservability(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("X-Holmes-Query-Key")
	if len(key) < 26 || len(key) > 128 {
		http.Error(w, "missing investigation", http.StatusUnauthorized)
		return
	}
	raw, err := s.db.Get(r.Context(), "telemost:holmes:"+key).Bytes()
	if errors.Is(err, redis.Nil) {
		http.Error(w, "unknown investigation", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "scope lookup unavailable", http.StatusServiceUnavailable)
		return
	}
	var scope queryScope
	if json.Unmarshal(raw, &scope) != nil {
		http.Error(w, "invalid scope", http.StatusForbidden)
		return
	}
	p, ok := s.sources[scope.Profile]
	if !ok || scope.Cluster != p.Cluster || scope.Namespace != p.Namespace || scope.Route != p.Route {
		http.Error(w, "profile changed", http.StatusForbidden)
		return
	}
	var input struct{ Query string }
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(&struct{}{}) != io.EOF || strings.TrimSpace(input.Query) == "" || len(input.Query) > 4096 {
		http.Error(w, "expected a query", 400)
		return
	}
	operation := r.PathValue("operation")
	if operation != "logs" && operation != "metrics" && operation != "names" {
		http.NotFound(w, r)
		return
	}
	if operation == "logs" && strings.Contains(input.Query, "|") {
		http.Error(w, "use a LogsQL row filter without pipes", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	start, end := scope.Observed.Add(-15*time.Minute).UTC().Format(time.RFC3339), scope.Observed.UTC().Format(time.RFC3339)
	result, err := p.read(ctx, s.holmes.client, operation, input.Query, start, end)
	if err != nil {
		http.Error(w, "source query failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"namespace": p.Namespace, "start": start, "end": end, "data": result})
}

func (p projectSource) read(ctx context.Context, client *http.Client, operation, expr, start, end string) (any, error) {
	params := url.Values{"start": {start}, "end": {end}}
	base := strings.TrimRight(p.Metrics.URL, "/") + "/api/v1/"
	var endpoint string
	switch operation {
	case "logs":
		endpoint = strings.TrimRight(p.Logs.URL, "/") + "/select/logsql/query"
		filter, _ := json.Marshal(map[string]string{"cluster": p.Cluster, "kubernetes.pod_namespace": p.Namespace})
		params.Set("extra_filters", string(filter))
		params.Set("limit", "10")
		params.Set("query", expr)
	case "names", "metrics":
		params.Set("extra_filters[]", "{cluster="+strconv.Quote(p.Cluster)+",namespace="+strconv.Quote(p.Namespace)+"}")
		if operation == "names" {
			endpoint = base + "label/__name__/values"
			params.Set("match[]", expr)
			params.Set("limit", "50")
		} else {
			endpoint = base + "query_range"
			params.Set("query", expr)
			params.Set("step", "60s")
			params.Set("limit", "10")
		}
	default:
		return nil, errors.New("unknown operation")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return nil, errors.New("invalid source endpoint")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if operation == "logs" {
		p.Logs.apply(req)
	} else {
		p.Metrics.Auth.apply(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("source unreachable or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	const max = 512 << 10
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || len(data) > max {
		return nil, errors.New("incomplete or oversized response; narrow the query")
	}
	if operation != "logs" {
		var v struct {
			Status string
			Data   json.RawMessage
		}
		if json.Unmarshal(data, &v) != nil || v.Status != "success" || len(v.Data) == 0 {
			return nil, errors.New("invalid metrics response")
		}
		return v.Data, nil
	}
	rows := make([]map[string]string, 0, 10)
	dec := json.NewDecoder(bytes.NewReader(data))
	for len(rows) < 10 {
		var fields map[string]json.RawMessage
		err := dec.Decode(&fields)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("invalid log response")
		}
		row := map[string]string{}
		for _, name := range []string{"_time", "level", "kubernetes.pod_name", "kubernetes.container_name", "_msg"} {
			var value string
			if json.Unmarshal(fields[name], &value) == nil {
				chars := []rune(value)
				row[name] = string(chars[:min(600, len(chars))])
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
