package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSources(t *testing.T) {
	t.Setenv("TEST_METRIC_TOKEN", "metric-token")
	t.Setenv("TEST_LOG_USER", "logs")
	t.Setenv("TEST_LOG_PASSWORD", "log-pass")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		project := path[0]
		switch project {
		case "alpha", "beta":
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("X-Holmes-Query-Key") != "" {
			t.Error("investigation credential forwarded")
		}
		if strings.HasSuffix(r.URL.Path, "/select/logsql/query") {
			expectedTenant := "1"
			if project == "alpha" {
				user, pass, ok := r.BasicAuth()
				if !ok || user != "logs" || pass != "log-pass" {
					t.Error("log credentials missing")
				}
			} else {
				expectedTenant = "2"
				if r.Header.Get("Authorization") != "" {
					t.Error("unexpected log credentials")
				}
			}
			if r.Header.Get("AccountID") != expectedTenant || r.Header.Get("ProjectID") != "0" || r.FormValue("extra_filters") != fmt.Sprintf(`{"cluster":"shared","kubernetes.pod_namespace":"%s"}`, project) {
				t.Error("incorrect log tenant/scope")
			}
			fmt.Fprintln(w, `{"_msg":"sample","private_field":"hidden"}`)
			return
		}
		if r.FormValue("extra_filters[]") != fmt.Sprintf(`{cluster="shared",namespace="%s"}`, project) {
			t.Error("incorrect metrics filter")
		}
		expected := ""
		if project == "alpha" {
			expected = "Bearer metric-token"
		}
		if r.Header.Get("Authorization") != expected || r.Header.Get("AccountID") != "" {
			t.Error("incorrect metric credentials")
		}
		fmt.Fprintln(w, `{"status":"success","data":["sample"]}`)
	}))
	defer upstream.Close()
	var config strings.Builder
	fmt.Fprintln(&config, "profiles:")
	for i, project := range []string{"alpha", "beta"} {
		fmt.Fprintf(&config, "  %s:\n    cluster: shared\n    namespace: %s\n    route: dev\n    metrics:\n      url: %s/%s/select/%d/prometheus\n", project, project, upstream.URL, project, i+1)
		if i == 0 {
			fmt.Fprintln(&config, "      auth: {bearerTokenEnv: TEST_METRIC_TOKEN}")
		}
		fmt.Fprintf(&config, "    logs:\n      url: %s/%s\n      accountID: '%d'\n      projectID: '0'\n", upstream.URL, project, i+1)
		if i == 0 {
			fmt.Fprintln(&config, "      auth: {usernameEnv: TEST_LOG_USER, passwordEnv: TEST_LOG_PASSWORD}")
		}
	}
	file := filepath.Join(t.TempDir(), "profiles.yaml")
	if err := os.WriteFile(file, []byte(config.String()), 0600); err != nil {
		t.Fatal(err)
	}
	var summary summarySettings
	sources, err := loadSources(file, &summary)
	if err != nil {
		t.Fatal(err)
	}
	if err = summary.validate(map[string]string{"dev": "test"}); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"alpha", "beta"} {
		for _, op := range []string{"logs", "metrics", "names"} {
			result, err := sources[project].read(context.Background(), upstream.Client(), op, "sample", "2026-01-01T00:00:00Z", "2026-01-01T00:15:00Z")
			if err != nil {
				t.Fatal(project, op, err)
			}
			if op == "logs" && (strings.Contains(fmt.Sprint(result), "hidden") || !strings.Contains(fmt.Sprint(result), "sample")) {
				t.Fatal(result)
			}
		}
	}
	t.Setenv("TEST_METRIC_TOKEN", "")
	if _, err := loadSources(file, &summarySettings{}); err == nil {
		t.Fatal("missing credential accepted")
	}
}
