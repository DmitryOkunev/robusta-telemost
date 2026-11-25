package main

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	chart "github.com/wcharczuk/go-chart/v2"
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
	Name string `json:"graph_name"`
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

func (f finding) message() string {
	status := "🔥 firing"
	if f.resolved() {
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
			img, err := b.Graph.png()
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

func (g graphData) png() ([]byte, error) {
	c := chart.Chart{Width: 1000, Height: 400, Title: g.Name, XAxis: chart.XAxis{ValueFormatter: func(v interface{}) string { return chart.TimeFromFloat64(v.(float64)).UTC().Format("15:04") }}}
	minX, maxX := math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, s := range g.Data.Series {
		if len(s.Timestamps) != len(s.Values) {
			return nil, errors.New("unaligned graph series")
		}
		var line chart.TimeSeries
		line.Name = fmt.Sprint(s.Metric)
		for i, raw := range s.Values {
			y, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsInf(y, 0) || math.IsNaN(y) {
				continue
			}
			sec, frac := math.Modf(s.Timestamps[i])
			line.XValues = append(line.XValues, time.Unix(int64(sec), int64(frac*1e9)).UTC())
			x := chart.TimeToFloat64(line.XValues[len(line.XValues)-1])
			minX, maxX = math.Min(minX, x), math.Max(maxX, x)
			minY, maxY = math.Min(minY, y), math.Max(maxY, y)
			line.YValues = append(line.YValues, y)
		}
		if len(line.XValues) > 0 {
			line.Style.DotWidth = 2
			c.Series = append(c.Series, line)
		}
	}
	if len(c.Series) == 0 {
		return nil, nil
	}
	if minX == maxX {
		pad := float64(time.Minute)
		c.XAxis.Range = &chart.ContinuousRange{Min: minX - pad, Max: maxX + pad}
	}
	if minY == maxY {
		pad := math.Max(math.Abs(minY)*0.05, 1e-6)
		c.YAxis.Range = &chart.ContinuousRange{Min: minY - pad, Max: maxY + pad}
	}
	if len(c.Series) > 1 {
		c.Elements = []chart.Renderable{chart.Legend(&c)}
	}
	var buf bytes.Buffer
	if err := c.Render(chart.PNG, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
