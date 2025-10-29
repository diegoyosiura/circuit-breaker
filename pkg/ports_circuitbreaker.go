package circuitbreaker

import "net/http"

type ICircuitBreaker interface {
	Do(req *http.Request, cl *http.Client) (*http.Response, error)
	Stop()
	Metrics() map[string]map[string]EndpointMetrics
}
