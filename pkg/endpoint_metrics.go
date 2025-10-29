package circuitbreaker

import "time"

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

	Ratio01Requests           int64 `json:"ratio_01_requests"`
	Ratio01SuccessfulRequests int64 `json:"ratio_01_successful_requests"`
	Ratio01FailedRequests     int64 `json:"ratio_01_failed_requests"`
	Ratio01Retry              int64 `json:"ratio_01_retry"`
	Ratio05Requests           int64 `json:"ratio_05_requests"`
	Ratio05SuccessfulRequests int64 `json:"ratio_05_successful_requests"`
	Ratio05FailedRequests     int64 `json:"ratio_05_failed_requests"`
	Ratio05Retry              int64 `json:"ratio_05_retry"`
	Ratio10Requests           int64 `json:"ratio_10_requests"`
	Ratio10SuccessfulRequests int64 `json:"ratio_10_successful_requests"`
	Ratio10FailedRequests     int64 `json:"ratio_10_failed_requests"`
	Ratio10Retry              int64 `json:"ratio_10_retry"`

	TimeRequests           []float64 `json:"-"`
	TimeSuccessfulRequests []float64 `json:"-"`
	TimeFailedRequests     []float64 `json:"-"`
	TimeRetry              []float64 `json:"-"`

	StartTimeRequests           []time.Time `json:"-"`
	StartTimeSuccessfulRequests []time.Time `json:"-"`
	StartTimeFailedRequests     []time.Time `json:"-"`
	StartTimeRetry              []time.Time `json:"-"`
}
