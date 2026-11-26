package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type finding struct {
	Title, Description, Severity, Fingerprint, Source string
	StartsAt                                          time.Time `json:"starts_at"`
	Subject                                           struct {
		Namespace string
		Labels    map[string]string
	}
	Links       []struct{ Name, URL string }
	Enrichments []struct {
		Blocks []struct {
			Hidden bool
			Text   string
			Graph  *graphData `json:"graph_data"`
		}
	}
}

type graphData struct {
	Name     string `json:"graph_name"`
	Metadata struct {
		Query string `json:"query"`
	} `json:"metadata"`
	Data struct {
		Series []struct {
			Metric     map[string]string
			Timestamps []float64
			Values     []string
		} `json:"series_list_result"`
	}
}

var robustaLink = regexp.MustCompile(`<(https?://[^|>\s]+)\|([^>]+)>`)

func (f finding) resolved() bool { return strings.HasPrefix(f.Title, "[RESOLVED] ") }

func (f finding) message() string { return f.messageWithStatus(f.resolved()) }

func (f finding) messageWithStatus(resolved bool) string {
	status := "🔥 firing"
	if resolved {
		status = "✅ resolved"
	}
	lines := []string{fmt.Sprintf("%s · %s · **%s**", status, f.Severity, strings.TrimPrefix(f.Title, "[RESOLVED] "))}
	if f.Subject.Namespace != "" {
		lines = append(lines, "Namespace: "+f.Subject.Namespace)
	}
	if f.Description != "" {
		lines = append(lines, f.Description)
	}
	for _, link := range f.Links {
		if strings.HasPrefix(link.URL, "https://") || strings.HasPrefix(link.URL, "http://") {
			lines = append(lines, "["+link.Name+"]("+link.URL+")")
		}
	}
	for _, enrichment := range f.Enrichments {
		for _, block := range enrichment.Blocks {
			if !block.Hidden && block.Text != "" {
				lines = append(lines, block.Text)
			}
		}
	}
	text := []rune(robustaLink.ReplaceAllString(strings.Join(lines, "\n"), "[$2]($1)"))
	if len(text) > 4096 {
		return string(text[:4095]) + "…"
	}
	return string(text)
}

func (f finding) graphs() ([][]byte, error) {
	var result [][]byte
	for _, e := range f.Enrichments {
		for _, b := range e.Blocks {
			if b.Hidden || b.Graph == nil {
				continue
			}
			if len(result) >= 8 {
				return nil, errors.New("too many graphs")
			}
			img, err := b.Graph.png(strings.TrimPrefix(f.Title, "[RESOLVED] "))
			if err != nil {
				return nil, err
			}
			if img != nil {
				result = append(result, img)
			}
		}
	}
	return result, nil
}
