package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

const maxBody = 4 << 20

func handler(s *service, routes map[string]string, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/findings/{route}", func(w http.ResponseWriter, r *http.Request) {
		received := time.Now()
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		chat, ok := routes[r.PathValue("route")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var f finding
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
		if err := d.Decode(&f); err != nil || d.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid or oversized JSON", 400)
			return
		}
		if f.Source != "PROMETHEUS" || f.Title == "" || f.Fingerprint == "" || f.StartsAt.IsZero() || f.Subject.Labels["robusta"] != "enabled" {
			http.Error(w, "expected an opted-in Prometheus finding with fingerprint and starts_at", 422)
			return
		}
		if f.Enrichments == nil {
			http.Error(w, "missing enrichments field; check the Robusta webhook size limit", 422)
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if err := s.deliver(ctx, chat, r.PathValue("route"), f, received); err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, errConflict) {
				status = http.StatusConflict
			}
			log.Printf("delivery %x: %v", incidentID(chat, f), err)
			http.Error(w, http.StatusText(status), status)
			return
		}
		w.WriteHeader(204)
	})
	if s.holmes != nil && len(s.sources) > 0 {
		mux.HandleFunc("POST /v1/observability/{operation}", s.queryObservability)
	}
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx).Err(); err != nil {
			http.Error(w, "Valkey unavailable", 503)
			return
		}
		w.WriteHeader(204)
	})
	return mux
}

func required(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing %s", key)
	}
	return v
}
func setting(key, fallback string) string {
	if s := os.Getenv(key); s != "" {
		return s
	}
	return fallback
}

func main() {
	var routes map[string]string
	if err := json.Unmarshal([]byte(required("TELEMOST_ROUTES")), &routes); err != nil || len(routes) == 0 {
		log.Fatal("TELEMOST_ROUTES must map route names to chat IDs")
	}
	for name, chat := range routes {
		if name == "" || strings.Contains(name, "/") || chat == "" {
			log.Fatal("invalid route or chat ID")
		}
	}
	var projectLabels map[string]string
	if err := json.Unmarshal([]byte(setting("TELEMOST_PROJECT_LABELS", "{}")), &projectLabels); err != nil || projectLabels == nil {
		log.Fatal("TELEMOST_PROJECT_LABELS must be a JSON object mapping project names to display labels")
	}
	for project, label := range projectLabels {
		if strings.TrimSpace(project) == "" || strings.TrimSpace(label) == "" || strings.ContainsAny(label, "\r\n") {
			log.Fatal("invalid project name or display label in TELEMOST_PROJECT_LABELS")
		}
	}
	db := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:           strings.Split(required("VALKEY_ADDRS"), ","),
		Username:        required("VALKEY_USERNAME"),
		Password:        required("VALKEY_PASSWORD"),
		Protocol:        2,
		DisableIdentity: true,
		MaxRetries:      -1,
	})
	defer db.Close()
	zone, err := time.LoadLocation(setting("INCIDENT_TIMEZONE", "UTC"))
	if err != nil {
		log.Fatal("invalid INCIDENT_TIMEZONE")
	}
	var summary summarySettings
	if err := json.Unmarshal([]byte(setting("INCIDENT_METRICS", "{}")), &summary); err != nil {
		log.Fatal("invalid INCIDENT_METRICS JSON")
	}
	sources, err := loadSources(os.Getenv("OBSERVABILITY_PROFILES_FILE"), &summary)
	if err != nil {
		log.Fatalf("invalid observability sources: %v", err)
	}
	if err := summary.validate(routes); err != nil {
		log.Fatal(err)
	}
	holmes, err := newHolmes(setting("HOLMES_CONFIG", "{}"), summary, sources)
	if err != nil {
		log.Fatalf("invalid HOLMES_CONFIG: %v", err)
	}
	s := &service{sources: sources, holmes: holmes, zone: zone, summary: summary,
		db:            db,
		projectLabels: projectLabels,
		bot: bot{
			base:  "https://botapi.messenger.yandex.net",
			token: required("YAMESSENGER_BOT_TOKEN"),
			client: &http.Client{
				Timeout:       12 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
		},
		firingReaction:   setting("FIRING_REACTION", "fire"),
		resolvedReaction: setting("RESOLVED_REACTION", "like"),
	}
	server := &http.Server{
		Addr:              setting("LISTEN_ADDR", ":8080"),
		Handler:           handler(s, routes, required("WEBHOOK_TOKEN")),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if holmes != nil {
		go s.runInvestigations(ctx)
	}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
		close(done)
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-done
}
