package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

type summaryProfile struct {
	Namespace, Cluster, Route, URL string
	Auth                           sourceAuth `json:"-"`
}
type signalSpec struct{ Series, Worst string }
type summarySettings struct {
	Profiles map[string]summaryProfile
	Signals  map[string]signalSpec
}

func (c summarySettings) validate(routes map[string]string) error {
	for k, p := range c.Profiles {
		u, err := url.Parse(p.URL)
		if k == "" || p.Namespace == "" || p.Cluster == "" || routes[p.Route] == "" || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("invalid summary profile %q", k)
		}
	}
	for name, s := range c.Signals {
		if name == "" || s.Series == "" || (s.Worst != "min" && s.Worst != "max") {
			return fmt.Errorf("invalid summary signal %q", name)
		}
	}
	return nil
}
func incidentDuration(d time.Duration) string {
	sec := int64(d / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%d с", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%d мин %d с", sec/60, sec%60)
	}
	return fmt.Sprintf("%d ч %d мин %d с", sec/3600, sec/60%60, sec%60)
}
func withSummary(caption, summary string) string {
	if summary == "" {
		return caption
	}
	head, body, _ := strings.Cut(caption, "\n")
	return limitMessage(strings.TrimSpace(head + "\n" + summary + "\n" + body))
}
func (s *service) resolution(ctx context.Context, route string, f finding) string {
	zone := s.zone
	if zone == nil {
		zone = time.UTC
	}
	start := f.StartsAt.In(zone)
	const layout = "02.01.2006 15:04:05 UTC-07:00"
	text := "Начало: " + start.Format(layout)
	if f.EndsAt.IsZero() || f.EndsAt.Before(f.StartsAt) {
		return text + "\nОкончание и длительность: нет данных"
	}
	end := f.EndsAt.In(zone)
	text += "\nОкончание: " + end.Format(layout) + "\nДлительность: " + incidentDuration(end.Sub(start))
	l := f.Subject.Labels
	p, ok := s.summary.Profiles[l["observability_profile"]]
	if !ok || p.Route != route || p.Namespace != f.Subject.Namespace || p.Namespace != l["namespace"] || p.Cluster != l["cluster"] {
		return text
	}
	signal, ok := s.summary.Signals[l["alertname"]]
	if !ok {
		return text
	}
	metrics, err := readSignal(ctx, p, signal, f)
	if err != nil {
		summaryReads.WithLabelValues("error").Inc()
		log.Printf("resolution measurements unavailable for %s: %v", l["alertname"], err)
		return text + "\nИзмерения: нет данных"
	}
	if metrics == "" {
		summaryReads.WithLabelValues("empty").Inc()
		return text + "\nИзмерения: нет данных"
	}
	summaryReads.WithLabelValues("ok").Inc()
	return text + "\n" + metrics
}

type instantSeries struct {
	Metric map[string]string
	Value  []json.RawMessage
}

func readSignal(ctx context.Context, p summaryProfile, spec signalSpec, f finding) (string, error) {
	// Only the expression comes from the rule. Connections always use the configured tenant.
	dashboard, err := url.Parse(f.Subject.Annotations["dashboard_graph_url"])
	if err != nil {
		return "", fmt.Errorf("invalid dashboard URL")
	}
	expr := dashboard.Query().Get("var-expression")
	if expr == "" {
		return "", nil
	}
	// Evaluate on a 1-minute grid across the incident and include both exact boundaries.
	// One instant request returns the full-window extreme and the closing reading independently.
	span := int64(f.EndsAt.Sub(f.StartsAt) / time.Second)
	agg := spec.Worst + "_over_time"
	query := fmt.Sprintf(`WITH (s = (%s)) label_set(s, "telemost_stat", "end") or label_set(s @ %.3f, "telemost_stat", "start")`, expr, float64(f.StartsAt.UnixMilli())/1000)
	if span >= 60 {
		query += fmt.Sprintf(` or label_set(%s((s)[%ds:60s]), "telemost_stat", "window")`, agg, span)
	}
	params := url.Values{"query": {query}, "time": {f.EndsAt.UTC().Format(time.RFC3339Nano)}}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(p.URL, "/")+"/api/v1/query", strings.NewReader(params.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	p.Auth.apply(req)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("query returned HTTP %d", resp.StatusCode)
	}
	var data struct {
		Status string
		Data   struct {
			ResultType string
			Result     []instantSeries
		}
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil || data.Status != "success" || data.Data.ResultType != "vector" {
		return "", fmt.Errorf("invalid query result")
	}
	var worst, last, threshold *float64
rows:
	for _, r := range data.Data.Result {
		if r.Metric["namespace"] != p.Namespace || len(r.Value) != 2 {
			continue
		}
		for _, key := range []string{"pod", "container", "persistentvolumeclaim"} {
			if want := f.Subject.Labels[key]; want != "" && r.Metric[key] != want {
				continue rows
			}
		}
		var value string
		if json.Unmarshal(r.Value[1], &value) != nil {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if strings.HasPrefix(r.Metric["job"], "Порог ") {
			threshold = &v
			continue
		}
		if r.Metric["job"] != spec.Series {
			continue
		}
		if r.Metric["telemost_stat"] == "end" {
			last = &v
		}
		if worst == nil || (spec.Worst == "max" && v > *worst) || (spec.Worst == "min" && v < *worst) {
			worst = &v
		}
	}
	if worst == nil {
		return "", nil
	}
	label := "Пик"
	if spec.Worst == "min" {
		label = "Минимум"
	}
	text := fmt.Sprintf("%s: %.1f%%", label, *worst)
	if last != nil {
		text += fmt.Sprintf(" · при закрытии: %.1f%%", *last)
	} else {
		text += " · при закрытии: нет данных"
	}
	if threshold != nil {
		text += fmt.Sprintf(" · порог: %g%%", *threshold)
	}
	return text, nil
}
