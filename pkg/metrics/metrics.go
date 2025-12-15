package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// HTTP Metrics
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "quirm_http_requests_total",
			Help: "Total number of HTTP requests processed.",
		},
		[]string{"method", "status", "path"},
	)
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "quirm_http_request_duration_seconds",
			Help:    "Duration of HTTP requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "status", "path"},
	)

	// Cache Metrics
	CacheOpsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "quirm_cache_ops_total",
			Help: "Total number of cache operations.",
		},
		[]string{"type"}, // hit or miss
	)

	// Processing Metrics
	ImageProcessDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "quirm_image_process_duration_seconds",
			Help:    "Duration of image processing.",
			Buckets: prometheus.DefBuckets,
		},
	)
	ImageProcessErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "quirm_image_process_errors_total",
			Help: "Total number of image processing errors.",
		},
	)

	// Storage Metrics
	S3FetchDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "quirm_s3_fetch_duration_seconds",
			Help:    "Duration of S3 fetch operations.",
			Buckets: prometheus.DefBuckets,
		},
	)
)

// Init registers all metrics with Prometheus
func Init() {
	prometheus.MustRegister(HTTPRequestsTotal)
	prometheus.MustRegister(HTTPRequestDuration)
	prometheus.MustRegister(CacheOpsTotal)
	prometheus.MustRegister(ImageProcessDuration)
	prometheus.MustRegister(ImageProcessErrorsTotal)
	prometheus.MustRegister(S3FetchDuration)
}
