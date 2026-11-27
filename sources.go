package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"

	"go.yaml.in/yaml/v2"
)

// Values name environment variables populated from a Secret. Credentials are
// validated at startup and never included in the Holmes request.
type sourceAuth struct {
	BearerTokenEnv string `yaml:"bearerTokenEnv"`
	UsernameEnv    string `yaml:"usernameEnv"`
	PasswordEnv    string `yaml:"passwordEnv"`
}

func (a sourceAuth) validate() error {
	if (a.UsernameEnv == "") != (a.PasswordEnv == "") || a.BearerTokenEnv != "" && a.UsernameEnv != "" {
		return fmt.Errorf("choose bearerTokenEnv or both usernameEnv and passwordEnv")
	}
	for _, name := range []string{a.BearerTokenEnv, a.UsernameEnv, a.PasswordEnv} {
		if name != "" && os.Getenv(name) == "" {
			return fmt.Errorf("missing credential environment variable %q", name)
		}
	}
	return nil
}
func (a sourceAuth) apply(req *http.Request) {
	if a.BearerTokenEnv != "" {
		req.Header.Set("Authorization", "Bearer "+os.Getenv(a.BearerTokenEnv))
	}
	if a.UsernameEnv != "" {
		req.SetBasicAuth(os.Getenv(a.UsernameEnv), os.Getenv(a.PasswordEnv))
	}
}

type metricsSource struct {
	URL  string
	Auth sourceAuth
}
type projectSource struct {
	Cluster, Namespace, Route string
	Metrics                   metricsSource
	Logs                      logSource
}

type sourceGroup struct {
	Namespaces     []string
	Cluster, Route string
	Metrics        metricsSource
	Logs           logSource
}

func loadSources(path string, groups []sourceGroup, summary *summarySettings) (map[string]projectSource, error) {
	var file struct{ Profiles map[string]projectSource }
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.UnmarshalStrict(b, &file); err != nil {
			return nil, fmt.Errorf("invalid observability profiles YAML: %w", err)
		}
		if len(file.Profiles) == 0 {
			return nil, fmt.Errorf("observability file has no profiles")
		}
	} else {
		file.Profiles = make(map[string]projectSource)
	}
	for _, group := range groups {
		if len(group.Namespaces) == 0 {
			return nil, fmt.Errorf("source group requires namespaces")
		}
		for _, ns := range group.Namespaces {
			if _, exists := file.Profiles[ns]; exists {
				return nil, fmt.Errorf("duplicate observability profile %q", ns)
			}
			file.Profiles[ns] = projectSource{Cluster: group.Cluster, Route: group.Route, Namespace: ns, Metrics: group.Metrics, Logs: group.Logs}
		}
	}
	if summary.Profiles == nil {
		summary.Profiles = make(map[string]summaryProfile)
	}
	for name, p := range file.Profiles {
		if _, exists := summary.Profiles[name]; exists {
			return nil, fmt.Errorf("profile %q already defined in INCIDENT_METRICS", name)
		}
		if !httpEndpoint(p.Metrics.URL) || !httpEndpoint(p.Logs.URL) {
			return nil, fmt.Errorf("invalid source URL in profile %q", name)
		}
		if (p.Logs.AccountID == "") != (p.Logs.ProjectID == "") {
			return nil, fmt.Errorf("profile %q requires both log tenant IDs or neither", name)
		}
		for _, v := range []string{p.Logs.AccountID, p.Logs.ProjectID} {
			if v != "" {
				if _, err := strconv.ParseUint(v, 10, 32); err != nil {
					return nil, fmt.Errorf("invalid log tenant in profile %q", name)
				}
			}
		}
		for _, auth := range []sourceAuth{p.Logs.Auth, p.Metrics.Auth} {
			if err := auth.validate(); err != nil {
				return nil, fmt.Errorf("profile %q: %w", name, err)
			}
		}
		summary.Profiles[name] = summaryProfile{Namespace: p.Namespace, Cluster: p.Cluster, Route: p.Route, URL: p.Metrics.URL, Auth: p.Metrics.Auth}
	}
	return file.Profiles, nil
}

func (s logSource) apply(req *http.Request) {
	s.Auth.apply(req)
	if s.AccountID != "" {
		req.Header.Set("AccountID", s.AccountID)
		req.Header.Set("ProjectID", s.ProjectID)
	}
}
