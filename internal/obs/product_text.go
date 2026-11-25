package obs

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type productTextMetrics struct {
	embedded, failed, retried                prometheus.Counter
	pending, orphaned, coverage, lastSuccess prometheus.Gauge
	state                                    *prometheus.GaugeVec
}

func newProductTextMetrics(reg *prometheus.Registry) productTextMetrics {
	const prefix = "gkg_product_text_"
	counter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: prefix + name + "_total", Help: help})
		reg.MustRegister(c)
		return c
	}
	gauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: prefix + name, Help: help})
		reg.MustRegister(g)
		return g
	}
	m := productTextMetrics{
		embedded:    counter("embedded", "Product text vectors committed."),
		failed:      counter("failed", "Individual product text failures recorded."),
		retried:     counter("retried", "Product text entries resubmitted after failures."),
		pending:     gauge("pending", "Distinct referenced product texts without a vector, including entries waiting to retry."),
		orphaned:    gauge("orphaned", "Product text vector nodes marked as orphaned."),
		coverage:    gauge("coverage", "Fraction of distinct referenced product texts with a vector."),
		lastSuccess: gauge("last_success_timestamp_seconds", "Unix timestamp of the latest product text vector commit."),
		state: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "worker_state", Help: "Current product text worker state and reason.",
		}, []string{"state", "reason"}),
	}
	reg.MustRegister(m.state)
	return m
}

// ProductTextBatch records individual outcomes after their graph commit.
func (m *Metrics) ProductTextBatch(embedded, failed int, now time.Time) {
	m.productText.embedded.Add(float64(embedded))
	m.productText.failed.Add(float64(failed))
	if embedded > 0 {
		m.productText.lastSuccess.Set(float64(now.Unix()))
	}
}

// ProductTextRetried counts entries sent again in a new embedding request.
func (m *Metrics) ProductTextRetried(n int) {
	m.productText.retried.Add(float64(n))
}

// ProductTextCoverage publishes coverage and GC gauges from the graph.
func (m *Metrics) ProductTextCoverage(referenced, embedded, orphaned int) {
	m.productText.pending.Set(float64(referenced - embedded))
	m.productText.orphaned.Set(float64(orphaned))
	var coverage float64
	if referenced > 0 {
		coverage = float64(embedded) / float64(referenced)
	}
	m.productText.coverage.Set(coverage)
}

// ProductTextWorkerState publishes the worker's current state and reason.
func (m *Metrics) ProductTextWorkerState(state, reason string) {
	m.productText.state.Reset()
	m.productText.state.WithLabelValues(state, reason).Set(1)
}
