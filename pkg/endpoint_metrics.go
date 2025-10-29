package circuitbreaker

// EndpointMetrics captures aggregated statistics for a host/endpoint combination.
type EndpointMetrics struct {
	TotalRequests      int64
	SuccessfulRequests int64
	FailedRequests     int64
	RetryCount         int64
}
