package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResolvedSummary(t *testing.T) {
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.FormValue("query")
		if !strings.Contains(q, "min_over_time((s)[1200s:60s])") || r.FormValue("time") != "2026-01-02T03:20:00Z" {
			t.Errorf("wrong query window: %s", q)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
  {"metric":{"namespace":"dev","persistentvolumeclaim":"disk","job":"Free, %","telemost_stat":"window"},"value":[1,"2.2"]},
  {"metric":{"namespace":"dev","persistentvolumeclaim":"other","job":"Free, %","telemost_stat":"window"},"value":[1,"0"]},
  {"metric":{"namespace":"dev","persistentvolumeclaim":"disk","job":"Free, %","telemost_stat":"end"},"value":[1,"13.0"]},
  {"metric":{"namespace":"dev","persistentvolumeclaim":"disk","job":"Порог 10 %","telemost_stat":"window"},"value":[1,"10"]}]}}`)
	}))
	defer backend.Close()
	f := sample(t)
	f.Subject.Namespace = "dev"
	f.Subject.Labels = map[string]string{"observability_profile": "dev", "namespace": "dev", "cluster": "cluster", "alertname": "PVC", "persistentvolumeclaim": "disk"}
	f.Subject.Annotations = map[string]string{"dashboard_graph_url": "https://example.invalid/d/test?var-expression=test"}
	f.EndsAt = f.StartsAt.Add(20 * time.Minute)
	zone, _ := time.LoadLocation("Europe/Samara")
	s := service{zone: zone, summary: summarySettings{Profiles: map[string]summaryProfile{"dev": {Namespace: "dev", Cluster: "cluster", Route: "test", URL: backend.URL}}, Signals: map[string]signalSpec{"PVC": {Series: "Free, %", Worst: "min"}}}}
	summary := s.resolution(context.Background(), "test", f)
	for _, piece := range []string{"07:00:00 UTC+04:00", "07:20:00 UTC+04:00", "20 мин 0 с", "Минимум: 2.2%", "при закрытии: 13.0%", "порог: 10%"} {
		if !strings.Contains(summary, piece) {
			t.Errorf("missing %s in %s", piece, summary)
		}
	}
	_ = s.resolution(context.Background(), "other", f)
	f.EndsAt = time.Time{}
	if text := s.resolution(context.Background(), "test", f); !strings.Contains(text, "нет данных") {
		t.Fatal(text)
	}
	if calls != 1 {
		t.Fatal("unexpected tenant queries", calls)
	}
}
