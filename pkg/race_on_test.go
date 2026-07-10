//go:build race

package circuitbreaker_test

// raceEnabled sinaliza que o binário de teste foi compilado com -race:
// testes de medição de tempo pulam (a instrumentação distorce o custo).
const raceEnabled = true
