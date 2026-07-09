package circuitbreaker_test

// Benchmarks de baseline (Fase 0) e gate de desempenho (Fase 2).
// Baseline @ v0.0.7 (medido): custo por Do() cresce de ~68µs para ~640µs/req
// em 10k requests (histórico ilimitado + sort sob o lock global).

import (
	"net/http"
	"testing"

	circuitbreaker "github.com/diegoyosiura/circuit-breaker/pkg"
)

func benchReq(b *testing.B) *http.Request {
	req, err := http.NewRequest(http.MethodGet, "http://bench.test/x", nil)
	if err != nil {
		b.Fatal(err)
	}
	return req
}

func BenchmarkDo_NoLimits(b *testing.B) {
	cb := circuitbreaker.NewCircuitBreaker("bench-serial", 0, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}
	req := benchReq(b)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cb.Do(req, cl)
		if err != nil {
			b.Fatal(err)
		}
		_ = resp.Body.Close()
	}
}

func BenchmarkDo_Parallel8(b *testing.B) {
	cb := circuitbreaker.NewCircuitBreaker("bench-par", 0, 0, 0, 0)
	defer cb.Stop()
	cl := &http.Client{Transport: &countingTransport{}}

	b.ReportAllocs()
	b.SetParallelism(8)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := benchReq(b)
		for pb.Next() {
			resp, err := cb.Do(req, cl)
			if err != nil {
				b.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	})
}
