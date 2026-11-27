package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"time"
)

var deliveryDelay = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "telemost_delivery_stage_seconds", Help: "Elapsed time to successful publication or reaction acknowledgement.", Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300},
}, []string{"stage", "event"})

var apiCalls = promauto.NewCounterVec(prometheus.CounterOpts{Name: "telemost_bot_api_requests_total", Help: "Bot API calls by method and result."}, []string{"method", "result"})
var summaryReads = promauto.NewCounterVec(prometheus.CounterOpts{Name: "telemost_summary_reads_total", Help: "Incident metric lookups by result."}, []string{"result"})

func observeDelay(stage string, resolved bool, start time.Time) {
	if start.IsZero() {
		return
	}
	event := "firing"
	if resolved {
		event = "resolved"
	}
	deliveryDelay.WithLabelValues(stage, event).Observe(max(0, time.Since(start).Seconds()))
}
