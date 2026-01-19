package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Gateway metrics
	InvocationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_invocations_total",
			Help: "Total number of function invocations",
		},
		[]string{"function_id", "status"},
	)

	InvocationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aether_invocation_duration_seconds",
			Help:    "Duration of function invocations",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"function_id"},
	)

	ColdStartsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_cold_starts_total",
			Help: "Total number of cold starts",
		},
		[]string{"function_id"},
	)

	ActiveRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "aether_active_requests",
			Help: "Number of currently active requests",
		},
	)

	InstanceDiscoveryDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "aether_instance_discovery_duration_seconds",
			Help:    "Duration of instance discovery from etcd",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
		},
	)

	QueuePublishErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "aether_queue_publish_errors_total",
			Help: "Total number of Redis queue publish errors",
		},
	)

	// Worker metrics
	InstancesActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aether_instances_active",
			Help: "Number of active function instances",
		},
		[]string{"function_id", "worker_id"},
	)

	VMSpawnsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_vm_spawns_total",
			Help: "Total number of VM spawns",
		},
		[]string{"function_id", "status"},
	)

	VMDeathsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_vm_deaths_total",
			Help: "Total number of unexpected VM deaths",
		},
		[]string{"function_id", "reason"},
	)

	CodeCacheHits = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "aether_code_cache_hits_total",
			Help: "Total number of code cache hits",
		},
	)

	CodeCacheMisses = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "aether_code_cache_misses_total",
			Help: "Total number of code cache misses",
		},
	)

	PortsAllocated = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "aether_ports_allocated",
			Help: "Number of currently allocated proxy ports",
		},
	)

	ScaleEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_scale_events_total",
			Help: "Total number of scaling events",
		},
		[]string{"function_id", "direction"},
	)

	VMSpawnDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "aether_vm_spawn_duration_seconds",
			Help:    "Duration of VM spawn operations",
			Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30},
		},
		[]string{"function_id"},
	)

	// Infrastructure metrics
	EtcdOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_etcd_operations_total",
			Help: "Total number of etcd operations",
		},
		[]string{"operation", "status"},
	)

	RedisOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_redis_operations_total",
			Help: "Total number of Redis operations",
		},
		[]string{"operation", "status"},
	)

	MinioOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "aether_minio_operations_total",
			Help: "Total number of MinIO operations",
		},
		[]string{"operation", "status"},
	)
)

// Init registers all metrics with Prometheus
func Init() {
	// Gateway metrics
	prometheus.MustRegister(InvocationsTotal)
	prometheus.MustRegister(InvocationDuration)
	prometheus.MustRegister(ColdStartsTotal)
	prometheus.MustRegister(ActiveRequests)
	prometheus.MustRegister(InstanceDiscoveryDuration)
	prometheus.MustRegister(QueuePublishErrors)

	// Worker metrics
	prometheus.MustRegister(InstancesActive)
	prometheus.MustRegister(VMSpawnsTotal)
	prometheus.MustRegister(VMDeathsTotal)
	prometheus.MustRegister(CodeCacheHits)
	prometheus.MustRegister(CodeCacheMisses)
	prometheus.MustRegister(PortsAllocated)
	prometheus.MustRegister(ScaleEventsTotal)
	prometheus.MustRegister(VMSpawnDuration)

	// Infrastructure metrics
	prometheus.MustRegister(EtcdOperationsTotal)
	prometheus.MustRegister(RedisOperationsTotal)
	prometheus.MustRegister(MinioOperationsTotal)
}

// Handler returns an HTTP handler for the /metrics endpoint
func Handler() http.Handler {
	return promhttp.Handler()
}

// RecordInvocation is a helper to record invocation metrics
func RecordInvocation(functionID, status string, duration time.Duration) {
	InvocationsTotal.WithLabelValues(functionID, status).Inc()
	InvocationDuration.WithLabelValues(functionID).Observe(duration.Seconds())
}
