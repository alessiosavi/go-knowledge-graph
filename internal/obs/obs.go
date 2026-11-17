// Package obs provides the observability primitives: a Prometheus
// registry with the pipeline counters and latencies, JSON structured logs
// and the request logging middleware. The HTTP probes that expose this
// state (/healthz, /readyz, /metrics) live on the api mux.
package obs

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the process registry with the pipeline counters and
// latencies. It structurally implements the ingest pipeline's metrics
// interface; a nil *Metrics is never wired (callers check before use).
type Metrics struct {
	reg *prometheus.Registry

	documents       *prometheus.CounterVec
	chunks          prometheus.Counter
	entities        prometheus.Counter
	facts           prometheus.Counter
	factsDropped    prometheus.Counter
	hygieneDropped  *prometheus.CounterVec
	hygieneRenamed  prometheus.Counter
	judgeVerdicts   *prometheus.CounterVec
	judgeFailures   prometheus.Counter
	judgeCache      *prometheus.CounterVec
	extractorErrors prometheus.Counter
	extractSeconds  prometheus.Histogram
	judgeSeconds    prometheus.Histogram
}

// New builds the registry and registers every metric.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		documents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "documents_total",
			Help: "Documents by pipeline status transition.",
		}, []string{"status"}),
		chunks: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chunks_total",
			Help: "Chunks written across processed documents.",
		}),
		entities: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "entities_total",
			Help: "Entities merged into the graph.",
		}),
		facts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "facts_total",
			Help: "Fact edges written across processed documents.",
		}),
		factsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "facts_dropped_total",
			Help: "Facts dropped below the store floor.",
		}),
		hygieneDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gkg_hygiene_dropped_total", Help: "Entity mentions dropped by hygiene rule.",
		}, []string{"rule"}),
		hygieneRenamed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gkg_hygiene_renamed_total", Help: "Entity mentions renamed by hygiene.",
		}),
		judgeVerdicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "judge_verdicts_total",
			Help: "Judge verdicts by status.",
		}, []string{"status"}),
		judgeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "judge_failures_total",
			Help: "Judge calls that failed (fact left unverified).",
		}),
		judgeCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "judge_cache_total",
			Help: "Verdict-cache lookups by result: hit, miss, read_error, write_error.",
		}, []string{"result"}),
		extractorErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "extractor_errors_total",
			Help: "Extractor calls that failed.",
		}),
		extractSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "extract_seconds",
			Help: "Extractor call latency in seconds.",
		}),
		judgeSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "judge_seconds",
			Help: "Judge verification latency in seconds, including cache lookups.",
		}),
	}
	reg.MustRegister(m.documents, m.chunks, m.entities, m.facts, m.factsDropped,
		m.hygieneDropped, m.hygieneRenamed, m.judgeVerdicts, m.judgeFailures, m.judgeCache, m.extractorErrors, m.extractSeconds, m.judgeSeconds)
	return m
}

// Registry returns the registry (test assertions and custom collectors).
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler serves the Prometheus scrape for the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// DocumentStatus counts one status transition of a document.
func (m *Metrics) DocumentStatus(status string) { m.documents.WithLabelValues(status).Inc() }

// Chunks counts written chunks.
func (m *Metrics) Chunks(n int) {
	if n > 0 {
		m.chunks.Add(float64(n))
	}
}

// Entities counts merged entities.
func (m *Metrics) Entities(n int) {
	if n > 0 {
		m.entities.Add(float64(n))
	}
}

// Facts counts typed fact edges written directly or promoted from candidates.
func (m *Metrics) Facts(n int) {
	if n > 0 {
		m.facts.Add(float64(n))
	}
}

// FactsDropped counts facts dropped below the store floor.
func (m *Metrics) FactsDropped(n int) {
	if n > 0 {
		m.factsDropped.Add(float64(n))
	}
}

// HygieneDropped counts entity mentions removed by one rule.
func (m *Metrics) HygieneDropped(rule string, n int) {
	if n > 0 {
		m.hygieneDropped.WithLabelValues(rule).Add(float64(n))
	}
}

// HygieneRenamed counts entity mentions whose name was normalized.
func (m *Metrics) HygieneRenamed(n int) {
	if n > 0 {
		m.hygieneRenamed.Add(float64(n))
	}
}

// JudgeVerdict counts one verdict by status.
func (m *Metrics) JudgeVerdict(status string) { m.judgeVerdicts.WithLabelValues(status).Inc() }

// JudgeFailure counts one failed judge call.
func (m *Metrics) JudgeFailure() { m.judgeFailures.Inc() }

// JudgeCache counts one verdict-cache result.
func (m *Metrics) JudgeCache(result string) { m.judgeCache.WithLabelValues(result).Inc() }

// ExtractorError counts one failed extractor call.
func (m *Metrics) ExtractorError() { m.extractorErrors.Inc() }

// ExtractSeconds observes one extractor call duration.
func (m *Metrics) ExtractSeconds(seconds float64) { m.extractSeconds.Observe(seconds) }

// JudgeSeconds observes one judge verification duration, including cache lookups.
func (m *Metrics) JudgeSeconds(seconds float64) { m.judgeSeconds.Observe(seconds) }

// Log returns the process logger: JSON records on stderr.
func Log() *slog.Logger { return LogTo(os.Stderr) }

// LogTo returns a JSON slog logger writing to w (test support).
func LogTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// RequestLogger logs one record per request: method, path (the query string
// is deliberately not logged), response status and duration. A nil logger
// falls back to Log().
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = Log()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds())
		})
	}
}

// statusRecorder captures the response status for the request log while
// staying transparent to the optional interfaces streaming handlers need
// (SSE via Flush, Hijack): WriteHeader records only the first call, Flush
// forwards directly and Unwrap lets http.ResponseController reach the
// underlying writer.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so streaming responses (SSE, the
// MCP streamable endpoint) keep working through the logging wrapper.
func (r *statusRecorder) Flush() {
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
