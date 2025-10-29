package circuitbreaker

// EndpointMetrics captures aggregated statistics for a host/endpoint combination.
type EndpointMetrics struct {
	TotalRequests      int64 `json:"total_requests"`
	SuccessfulRequests int64 `json:"successful_requests"`
	FailedRequests     int64 `json:"failed_requests"`
	RetryCount         int64 `json:"retry_count"`

	MeanRequests           float64 `json:"mean_requests"`
	MeanSuccessfulRequests float64 `json:"mean_successful_requests"`
	MeanFailedRequests     float64 `json:"mean_failed_requests"`
	MeanRetry              float64 `json:"mean_retry_count"`

	TimeRequests           []float64 `json:"-"`
	TimeSuccessfulRequests []float64 `json:"-"`
	TimeFailedRequests     []float64 `json:"-"`
	TimeRetry              []float64 `json:"-"`
}
