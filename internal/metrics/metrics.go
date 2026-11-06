// Package metrics exports the server's metrics for Prometheus. Every method
// works on a nil *Metrics and does nothing, so metrics can be turned off.
package metrics

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/VizzleTF/arcomium-sync/internal/store"
	pb "github.com/VizzleTF/arcomium-sync/internal/syncpb"
)

const namespace = "arcomium_sync"

// Metrics holds the server's metrics.
type Metrics struct {
	registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	entries   *prometheus.CounterVec
	limited   *prometheus.CounterVec
	evicted   prometheus.Counter
	collected *prometheus.CounterVec
	inFlight  prometheus.Gauge
}

// New returns metrics with the Go runtime's and the process's.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "requests_total",
			Help: "Requests by route, sync message and result: the protocol error code, or the HTTP status.",
		}, []string{"route", "message", "result"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "request_duration_seconds",
			Help:    "Time to answer sync protocol requests.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"message"}),
		entries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "commit_entries_total",
			Help: "Committed entities by answer.",
		}, []string{"result"}),
		limited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "limited_total",
			Help: "Requests refused by a limit, by limit.",
		}, []string{"limit"}),
		evicted: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "history_evicted_total",
			Help: "History entities removed to make room in a chain.",
		}),
		collected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "collected_total",
			Help: "What the retention job removed: tombstones, expired entities and unused chains.",
		}, []string{"kind"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "requests_in_flight",
			Help: "Sync protocol requests being handled.",
		}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.entries, m.limited, m.evicted, m.collected, m.inFlight,
	)
	return m
}

// Handler serves the metrics.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Gauge adds a gauge whose value value returns at each scrape.
func (m *Metrics) Gauge(name, help string, value func() float64) {
	if m == nil {
		return
	}
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Name: name, Help: help,
	}, value))
}

// Pool adds the connection pool's statistics.
func (m *Metrics) Pool(pool *pgxpool.Pool) {
	if m == nil {
		return
	}
	m.registry.MustRegister(poolCollector{pool})
}

// Request counts a finished request.
func (m *Metrics) Request(route, message, result string) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(route, message, result).Inc()
}

// Handled records how long a protocol request took and how its commit
// entries were answered.
func (m *Metrics) Handled(message string, took time.Duration, rsp *pb.ClientToServerResponse) {
	if m == nil {
		return
	}
	m.duration.WithLabelValues(message).Observe(took.Seconds())
	for _, entry := range rsp.GetCommit().GetEntryresponse() {
		m.entries.WithLabelValues(entry.GetResponseType().String()).Inc()
	}
}

// Started and Finished track the requests in flight.
func (m *Metrics) Started() {
	if m != nil {
		m.inFlight.Inc()
	}
}

func (m *Metrics) Finished() {
	if m != nil {
		m.inFlight.Dec()
	}
}

// Limited counts a request a limit refused.
func (m *Metrics) Limited(limit string) {
	if m == nil {
		return
	}
	m.limited.WithLabelValues(limit).Inc()
}

// Evicted counts history entities removed to make room.
func (m *Metrics) Evicted(n int64) {
	if m == nil {
		return
	}
	m.evicted.Add(float64(n))
}

// Collected counts what the retention job removed.
func (m *Metrics) Collected(c store.Collected) {
	if m == nil {
		return
	}
	m.collected.WithLabelValues("tombstones").Add(float64(c.Tombstones))
	m.collected.WithLabelValues("expired").Add(float64(c.Expired))
	m.collected.WithLabelValues("empty_chains").Add(float64(c.EmptyChains))
	m.collected.WithLabelValues("inactive_chains").Add(float64(c.InactiveChains))
}

// poolCollector reports pgxpool statistics.
type poolCollector struct{ pool *pgxpool.Pool }

var (
	poolConns = prometheus.NewDesc(namespace+"_db_connections",
		"Database connections by state.", []string{"state"}, nil)
	poolMax = prometheus.NewDesc(namespace+"_db_connections_max",
		"The most database connections the pool opens.", nil, nil)
	poolWaits = prometheus.NewDesc(namespace+"_db_acquire_waits_total",
		"Times a request waited for a database connection.", nil, nil)
	poolWait = prometheus.NewDesc(namespace+"_db_acquire_wait_seconds_total",
		"Time requests waited for a database connection.", nil, nil)
)

func (c poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolConns
	ch <- poolMax
	ch <- poolWaits
	ch <- poolWait
}

func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	stat := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(stat.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(poolConns, prometheus.GaugeValue, float64(stat.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(poolMax, prometheus.GaugeValue, float64(stat.MaxConns()))
	ch <- prometheus.MustNewConstMetric(poolWaits, prometheus.CounterValue, float64(stat.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(poolWait, prometheus.CounterValue, stat.EmptyAcquireWaitTime().Seconds())
}
