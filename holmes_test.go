package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestHolmesThread(t *testing.T) {
	addr := os.Getenv("TEST_VALKEY_ADDR")
	if addr == "" {
		t.Skip("requires disposable Valkey cluster")
	}
	db := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, Username: os.Getenv("TEST_VALKEY_USER"), Password: os.Getenv("TEST_VALKEY_PASSWORD"), Protocol: 2, DisableIdentity: true})
	defer db.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	posted := make(chan int64, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/select/logsql/query":
			if q := r.FormValue("query"); !strings.Contains(q, `kubernetes.pod_namespace:="sample-dev"`) || !strings.Contains(q, `kubernetes.pod_name:="pod1"`) || r.Header.Get("AccountID") != "1" || r.Header.Get("ProjectID") != "0" {
				t.Error("incorrect tenant or scope", q)
			}
			fmt.Fprint(w, `{"total":"20","errors":"2","warnings":"1"}`)
		case "/api/chat":
			close(started)
			var req struct{ Ask string }
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			var e incidentEvidence
			if err := json.Unmarshal([]byte(req.Ask), &e); err != nil || e.Alert != "NewUnconfiguredRule" || e.Description != "CPU is high" || e.Logs == nil || e.Logs.Errors != 2 || len(e.Graphs) != 1 || e.Graphs[0].Max != 1 || strings.Contains(req.Ask, "PRIVATE") {
				t.Error("unexpected evidence", req.Ask)
			}
			<-release
			fmt.Fprint(w, `{"analysis":"Проверить метрику"}`)
		case "/bot/v1/messages/sendGallery/":
			fmt.Fprint(w, `{"ok":true,"message_id":20,"images":[{"file_id":"f","width":10,"height":10}]}`)
		case "/bot/v1/messages/sendText/":
			var v struct {
				Thread int64 `json:"thread_id"`
				Text   string
			}
			json.NewDecoder(r.Body).Decode(&v)
			if !strings.Contains(v.Text, "HolmesGPT") {
				t.Error(v.Text)
			}
			posted <- v.Thread
			fmt.Fprint(w, `{"ok":true,"message_id":22}`)
		case "/bot/v1/messages/sendReaction/":
			fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	cfg := summarySettings{Profiles: map[string]summaryProfile{"sample-dev": {Cluster: "one", Namespace: "sample-dev", Route: "dev"}}, Signals: map[string]signalSpec{"CPU": {Series: "CPU", Worst: "max"}}}
	h, err := newHolmes(fmt.Sprintf(`{"enabled":true,"endpoint":%q,"profiles":{"sample-dev":{"url":%q,"accountID":"1","projectID":"0"}}}`, backend.URL, backend.URL), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := service{holmes: h, summary: cfg, db: db, bot: bot{backend.URL, "x", backend.Client()}}
	f := sample(t)
	f.Fingerprint = fmt.Sprint(time.Now().UnixNano())
	f.Description = "CPU is high"
	f.Subject.Annotations = map[string]string{"internal": "PRIVATE"}
	f.Subject.Namespace = "sample-dev"
	f.Subject.Labels = map[string]string{"alertname": "NewUnconfiguredRule", "cluster": "one", "namespace": "sample-dev", "observability_profile": "sample-dev", "pod": "pod1"}
	f.Enrichments[0].Blocks[0].Graph.Data.Series[0].Metric = map[string]string{"job": "CPU"}
	s.queueInvestigation("k", "c", "prod", 20, f, time.Now())
	if len(h.jobs) != 0 {
		t.Fatal("cross-route job")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runInvestigations(ctx)
	if err = s.deliver(ctx, "chat", "dev", f, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no investigation")
	}
	if err = s.deliver(ctx, "chat", "dev", f, time.Now()); err != nil || len(h.jobs) != 0 {
		t.Fatal("duplicate job", err)
	}
	close(release)
	select {
	case id := <-posted:
		if id != 20 {
			t.Fatal("wrong thread", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no analysis")
	}
}

func TestAllProfileAlerts(t *testing.T) {
	cfg := summarySettings{Profiles: map[string]summaryProfile{"demo-dev": {Route: "dev", Cluster: "one", Namespace: "demo-dev"}}}
	h := &holmesWorker{config: holmesConfig{Profiles: map[string]logSource{"demo-dev": {}}, ContextLabels: []string{"market_id", "source_id", "deployment", "reason", "job"}}, jobs: make(chan investigation, 1)}
	s := service{holmes: h, summary: cfg}
	f := sample(t)
	f.Title = "No quotes"
	f.Description = "Quotes are missing. See https://example.invalid/private"
	f.Severity = "HIGH"
	f.Subject.Namespace = "demo-dev"
	f.Subject.Labels = map[string]string{"cluster": "one", "namespace": "demo-dev", "observability_profile": "demo-dev", "alertname": "FutureQuoteRule", "market_id": "X_Y", "private_label": "PRIVATE"}
	f.Enrichments = nil
	collect := func() incidentEvidence {
		t.Helper()
		s.queueInvestigation("key", "chat", "dev", 7, f, time.Now())
		select {
		case j := <-h.jobs:
			return j.evidence
		default:
			t.Fatal("expected investigation")
			return incidentEvidence{}
		}
	}
	e := collect()
	if e.Alert != "FutureQuoteRule" || e.Title != "No quotes" || e.Description != "Quotes are missing. See [URL]" || e.Severity != "HIGH" || e.Labels["market_id"] != "X_Y" || len(e.Labels) != 1 || len(e.Graphs) != 0 {
		t.Fatal(e)
	}

	f.Enrichments = sample(t).Enrichments
	g := f.Enrichments[0].Blocks[0].Graph
	signal := g.Data.Series[0]
	signal.Metric = map[string]string{"market_id": "X_Y", "__name__": "quotes", "job": "oracle", "private_label": "PRIVATE"}
	foreign := signal
	foreign.Metric = map[string]string{"market_id": "another"}
	g.Data.Series = nil
	g.Data.Series = append(g.Data.Series, foreign)
	for i := 0; i < 9; i++ {
		g.Data.Series = append(g.Data.Series, signal)
	}
	e = collect()
	if len(e.Graphs) != 8 || !e.GraphsTruncated || e.Graphs[0].Name != "quotes" || e.Graphs[0].Labels["job"] != "oracle" || e.Graphs[0].Labels["market_id"] != "X_Y" {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(e)
	if strings.Contains(string(payload), "PRIVATE") {
		t.Fatal("unexpected label in evidence")
	}
	f.Subject.Labels["namespace"] = "demo-prod"
	s.queueInvestigation("k", "chat", "dev", 7, f, time.Now())
	if len(h.jobs) != 0 {
		t.Fatal("mismatched namespace accepted")
	}
}
