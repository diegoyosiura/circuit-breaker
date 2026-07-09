package circuitbreaker

import "time"

// Estado interno de métricas por endpoint (Fase 2 do PLANO.md).
//
// Substitui os slices de crescimento ilimitado por estruturas de custo O(1)
// por registro, preservando a semântica dos campos exportados de
// EndpointMetrics (travada pelos testes de característica T0.1/T0.4):
//   - Mean*  = média das últimas ringSize amostras (ring buffer fixo);
//   - Ratio* = contagem de eventos na janela de 1/5/10 min, calculada no
//     momento do REGISTRO (fica "stale" até o próximo evento — semântica
//     original preservada) a partir de uma roda de buckets de 1s;
//   - Time*/StartTime* exportados = amostra das últimas ringSize entradas.
//
// Baseline @ v0.0.7: 68→640 µs/req crescentes e +137 B/req retidos;
// alvo: custo estável e retenção O(1) por endpoint.

const (
	ringSize     = 20  // amostras das médias (semântica "últimos 20")
	wheelSeconds = 600 // maior janela de ratio: 10 minutos
)

// floatRing mantém as últimas ringSize amostras com soma incremental.
type floatRing struct {
	vals  [ringSize]float64
	n     int // total de inserções (índice lógico)
	total float64
}

func (r *floatRing) add(v float64) {
	idx := r.n % ringSize
	if r.n >= ringSize {
		r.total -= r.vals[idx]
	}
	r.vals[idx] = v
	r.total += v
	r.n++
}

func (r *floatRing) mean() float64 {
	c := min(r.n, ringSize)
	if c == 0 {
		return 0
	}
	return r.total / float64(c)
}

// slice devolve as amostras em ordem cronológica (cópia nova a cada chamada —
// o snapshot de Metrics() nunca aliasa o estado vivo).
func (r *floatRing) slice() []float64 {
	c := min(r.n, ringSize)
	out := make([]float64, 0, c)
	for i := r.n - c; i < r.n; i++ {
		out = append(out, r.vals[i%ringSize])
	}
	return out
}

// timeRing é o equivalente de floatRing para instantes de início.
type timeRing struct {
	vals [ringSize]time.Time
	n    int
}

func (r *timeRing) add(t time.Time) {
	r.vals[r.n%ringSize] = t
	r.n++
}

func (r *timeRing) slice() []time.Time {
	c := min(r.n, ringSize)
	out := make([]time.Time, 0, c)
	for i := r.n - c; i < r.n; i++ {
		out = append(out, r.vals[i%ringSize])
	}
	return out
}

// bucketWheel conta eventos por segundo numa roda circular de wheelSeconds
// posições — contagem de janela em O(window) constante, sem reter timestamps.
type bucketWheel struct {
	buckets [wheelSeconds]int32
	lastSec int64 // unix second do último avanço
}

func (w *bucketWheel) advance(sec int64) {
	if w.lastSec == 0 {
		w.lastSec = sec
		return
	}
	if sec <= w.lastSec {
		return
	}
	gap := sec - w.lastSec
	if gap >= wheelSeconds {
		clear(w.buckets[:])
	} else {
		for s := w.lastSec + 1; s <= sec; s++ {
			w.buckets[s%wheelSeconds] = 0
		}
	}
	w.lastSec = sec
}

func (w *bucketWheel) record(t time.Time) {
	sec := t.Unix()
	w.advance(sec)
	if sec == w.lastSec { // dentro da roda (eventos antigos demais são ignorados)
		w.buckets[sec%wheelSeconds]++
	}
}

func (w *bucketWheel) countLast(now time.Time, seconds int64) int64 {
	sec := now.Unix()
	w.advance(sec)
	var n int64
	for s := sec - seconds + 1; s <= sec; s++ {
		if s > 0 {
			n += int64(w.buckets[s%wheelSeconds])
		}
	}
	return n
}

// familyState agrega uma família de eventos (requests, sucessos, falhas ou
// retries): médias, amostras recentes e contagens de janela.
type familyState struct {
	durations floatRing
	starts    timeRing
	wheel     bucketWheel

	mean                      float64
	ratio01, ratio05, ratio10 int64
}

func (f *familyState) record(start, end time.Time) {
	f.durations.add(end.Sub(start).Seconds())
	f.starts.add(start)
	f.wheel.record(start)
	f.mean = f.durations.mean()

	// Ratios calculados no momento do registro (semântica original: valem
	// "até o próximo evento", não são recalculados no snapshot).
	now := time.Now()
	f.ratio01 = f.wheel.countLast(now, 60)
	f.ratio05 = f.wheel.countLast(now, 300)
	f.ratio10 = f.wheel.countLast(now, 600)
}

// endpointState é o estado vivo de um host/endpoint (e do agregado ::root).
type endpointState struct {
	totalRequests          int64
	successfulRequests     int64
	failedRequests         int64
	retryCount             int64
	tokenWaitCancellations int64

	req     familyState
	success familyState
	failure familyState
	retry   familyState
}

// snapshot materializa o EndpointMetrics público — todos os campos são
// cópias novas; nenhum aliasing com o estado vivo.
func (s *endpointState) snapshot() EndpointMetrics {
	return EndpointMetrics{
		TotalRequests:          s.totalRequests,
		SuccessfulRequests:     s.successfulRequests,
		FailedRequests:         s.failedRequests,
		RetryCount:             s.retryCount,
		TokenWaitCancellations: s.tokenWaitCancellations,

		MeanRequests:           s.req.mean,
		MeanSuccessfulRequests: s.success.mean,
		MeanFailedRequests:     s.failure.mean,
		MeanRetry:              s.retry.mean,

		Ratio01Requests:           s.req.ratio01,
		Ratio01SuccessfulRequests: s.success.ratio01,
		Ratio01FailedRequests:     s.failure.ratio01,
		Ratio01Retry:              s.retry.ratio01,
		Ratio05Requests:           s.req.ratio05,
		Ratio05SuccessfulRequests: s.success.ratio05,
		Ratio05FailedRequests:     s.failure.ratio05,
		Ratio05Retry:              s.retry.ratio05,
		Ratio10Requests:           s.req.ratio10,
		Ratio10SuccessfulRequests: s.success.ratio10,
		Ratio10FailedRequests:     s.failure.ratio10,
		Ratio10Retry:              s.retry.ratio10,

		TimeRequests:           s.req.durations.slice(),
		TimeSuccessfulRequests: s.success.durations.slice(),
		TimeFailedRequests:     s.failure.durations.slice(),
		TimeRetry:              s.retry.durations.slice(),

		StartTimeRequests:           s.req.starts.slice(),
		StartTimeSuccessfulRequests: s.success.starts.slice(),
		StartTimeFailedRequests:     s.failure.starts.slice(),
		StartTimeRetry:              s.retry.starts.slice(),
	}
}
