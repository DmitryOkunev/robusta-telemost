package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func sample(t *testing.T) finding {
	t.Helper()
	path := setting("TEST_FINDING_FILE", "testdata/firing.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f finding
	if err = json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWebhookGraph(t *testing.T) {
	f := sample(t)
	images, err := f.graphs()
	if err != nil || len(images) != 1 {
		t.Fatalf("graphs %d: %v", len(images), err)
	}
	if _, err := png.Decode(bytes.NewReader(images[0])); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.message(), "](https://") {
		t.Fatal("missing markdown link")
	}
}

func TestRejectUnroutedFinding(t *testing.T) {
	h := handler(nil, map[string]string{"dev": "chat-a"}, "test")
	for _, tc := range []struct {
		path, key string
		code      int
	}{{"dev", "wrong", 401}, {"missing", "test", 404}, {"dev", "test", 422}} {
		req := httptest.NewRequest("POST", "/v1/findings/"+tc.path, strings.NewReader(`{"title":"uncategorized"}`))
		req.Header.Set("Authorization", "Bearer "+tc.key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatal(w.Code)
		}
	}
}

func TestThreadLifecycle(t *testing.T) {
	addr := os.Getenv("TEST_VALKEY_ADDR")
	if addr == "" {
		t.Skip("set TEST_VALKEY_ADDR to a disposable Valkey cluster")
	}
	const parent = int64(9007199254740993)
	var mu sync.Mutex
	var calls []string
	failReaction := true
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "OAuth fixture" {
			t.Error("bot auth")
		}
		switch r.URL.Path {
		case "/bot/v1/messages/sendGallery/":
			if err := r.ParseMultipartForm(4 << 20); err != nil {
				t.Error(err)
				return
			}
			file, _, err := r.FormFile("images")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			if _, err = png.Decode(file); err != nil {
				t.Error(err)
			}
			if !strings.Contains(r.FormValue("text"), "](https://") {
				t.Error("caption lost link")
			}
			calls = append(calls, r.FormValue("chat_id")+":gallery:"+r.FormValue("thread_id"))
			fmt.Fprintf(w, `{"ok":true,"message_id":%d}`, parent)
		case "/bot/v1/messages/sendReaction/":
			var msg struct {
				Chat     string `json:"chat_id"`
				ID       int64  `json:"message_id"`
				Reaction map[string]string
			}
			if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
				t.Error(err)
				return
			}
			if msg.ID != parent || msg.Reaction["type"] != "default_reaction" {
				t.Error("incorrect reaction target")
			}
			if failReaction {
				failReaction = false
				http.Error(w, `{"ok":false}`, 503)
				return
			}
			calls = append(calls, msg.Chat+":"+msg.Reaction["name"])
			fmt.Fprint(w, `{"ok":true}`)
		default:
			t.Error("unexpected API path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	newHandler := func() http.Handler {
		db := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, Protocol: 2, Username: os.Getenv("TEST_VALKEY_USER"), Password: os.Getenv("TEST_VALKEY_PASSWORD"), DisableIdentity: true})
		t.Cleanup(func() { db.Close() })
		return handler(&service{db: db, bot: bot{api.URL, "fixture", api.Client()}, firingReaction: "fire", resolvedReaction: "like"}, map[string]string{"dev": "channel-a", "test": "channel-b"}, "key")
	}
	f := sample(t)
	f.Fingerprint = fmt.Sprintf("integration-%d", time.Now().UnixNano())
	post := func(h http.Handler, route string, f finding) int {
		payload, _ := json.Marshal(f)
		req := httptest.NewRequest("POST", "/v1/findings/"+route, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	first := newHandler()
	if got := post(first, "dev", f); got != 502 {
		t.Fatal("reaction failure", got)
	}
	second := newHandler()
	for _, route := range []string{"dev", "test", "dev"} {
		if got := post(second, route, f); got != 204 {
			t.Fatal("firing", got)
		}
	}
	resolved := f
	resolved.Title = "[RESOLVED] " + f.Title
	for _, event := range []finding{resolved, resolved, f} {
		if got := post(second, "dev", event); got != 204 {
			t.Fatal("status", got)
		}
	}
	if got := post(second, "test", resolved); got != 204 {
		t.Fatal("second channel", got)
	}
	want := "channel-a:gallery:,channel-a:fire,channel-b:gallery:,channel-b:fire,channel-a:gallery:" + strconv.FormatInt(parent, 10) + ",channel-a:like,channel-b:gallery:" + strconv.FormatInt(parent, 10) + ",channel-b:like"
	if strings.Join(calls, ",") != want {
		t.Fatalf("calls %v", calls)
	}
}

// A definite rejection can be retried. A timeout after sending the body needs
// reconciliation because the bot may have accepted the message.
func TestUncertainAndRejectedPosts(t *testing.T) {
	addr := os.Getenv("TEST_VALKEY_ADDR")
	if addr == "" {
		t.Skip("requires a disposable Valkey cluster")
	}
	db := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}, Protocol: 2, Username: os.Getenv("TEST_VALKEY_USER"), Password: os.Getenv("TEST_VALKEY_PASSWORD"), DisableIdentity: true})
	defer db.Close()
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", ambiguous), func(t *testing.T) {
			var attempts atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/bot/v1/messages/sendReaction/" {
					fmt.Fprint(w, `{"ok":true}`)
					return
				}
				if r.URL.Path != "/bot/v1/messages/sendText/" {
					t.Error(r.URL.Path)
				}
				n := attempts.Add(1)
				if n == 1 {
					if ambiguous {
						time.Sleep(150 * time.Millisecond)
						return
					}
					w.WriteHeader(429)
					fmt.Fprint(w, `{"ok":false}`)
					return
				}
				fmt.Fprint(w, `{"ok":true,"message_id":12345}`)
			}))
			defer api.Close()
			client := api.Client()
			client.Timeout = 50 * time.Millisecond
			s := service{db: db, bot: bot{api.URL, "fixture", client}, firingReaction: "fire", resolvedReaction: "like"}
			f := sample(t)
			f.Enrichments = nil
			f.Fingerprint = fmt.Sprint(time.Now().UnixNano())
			orphan := f
			orphan.Title = "[RESOLVED] " + f.Title
			if err := s.deliver(context.Background(), "chat", orphan); !errors.Is(err, errConflict) {
				t.Fatalf("orphan: %v", err)
			}
			if err := s.deliver(context.Background(), "chat", f); err == nil {
				t.Fatal("expected bot error")
			}
			err := s.deliver(context.Background(), "chat", f)
			if ambiguous {
				if !errors.Is(err, errConflict) || attempts.Load() != 1 {
					t.Fatalf("ambiguous retry: %v, calls %d", err, attempts.Load())
				}
			} else if err != nil || attempts.Load() != 2 {
				t.Fatalf("definite retry: %v, calls %d", err, attempts.Load())
			}
		})
	}
}
