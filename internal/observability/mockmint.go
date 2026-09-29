package observability

// Metrics are mockmint's metric series. A nil *Metrics disables recording,
// so components can be used without observability wiring (tests, validate).
type Metrics struct {
	Registry *Registry

	HTTPRequests *CounterVec   // package, operation, status
	HTTPDuration *HistogramVec // package, operation
	AMQPMessages *CounterVec   // package, operation, outcome
	AMQPPublish  *CounterVec   // package, operation, result
	AMQPUp       *GaugeVec
	Packages     *GaugeVec
	Reloads      *CounterVec // result
}

// NewMetrics registers mockmint's metrics in a new registry.
func NewMetrics() *Metrics {
	r := NewRegistry()
	return &Metrics{
		Registry:     r,
		HTTPRequests: r.NewCounterVec("mockmint_http_requests_total", "HTTP requests served, by package, operation and status.", "package", "operation", "status"),
		HTTPDuration: r.NewHistogramVec("mockmint_http_request_duration_seconds", "HTTP request duration including injected latency.", DefBuckets, "package", "operation"),
		AMQPMessages: r.NewCounterVec("mockmint_amqp_messages_total", "Consumed messages, by package, operation and outcome (ack, reject, requeue).", "package", "operation", "outcome"),
		AMQPPublish:  r.NewCounterVec("mockmint_amqp_published_total", "Published messages, by package, operation and result (ok, error).", "package", "operation", "result"),
		AMQPUp:       r.NewGaugeVec("mockmint_amqp_connected", "1 while connected to RabbitMQ."),
		Packages:     r.NewGaugeVec("mockmint_packages_loaded", "Packages currently served."),
		Reloads:      r.NewCounterVec("mockmint_reloads_total", "Package reloads, by result (ok, error).", "result"),
	}
}
