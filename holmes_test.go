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
			if err := json.Unmarshal([]byte(req.Ask), &e); err != nil || e.Logs == nil || e.Logs.Errors != 2 || len(e.Graphs) != 1 || e.Graphs[0].Max != 1 || strings.Contains(req.Ask, "PRIVATE") {
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
	f.Description = "PRIVATE"
	f.Subject.Namespace = "sample-dev"
	f.Subject.Labels = map[string]string{"alertname": "CPU", "cluster": "one", "namespace": "sample-dev", "observability_profile": "sample-dev", "pod": "pod1"}
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
