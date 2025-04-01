package circuitbreaker_test

import (
	"circuit-breaker/pkg"
	"testing"
)

func TestManager_CreatesAndGetsCircuitBreaker(t *testing.T) {
	manager := circuitbreaker.NewManager()

	cb1 := manager.NewCircuitBreaker("test-service", 10, 50, 30, 3)
	if cb1 == nil {
		t.Fatal("Expected non-nil CircuitBreaker")
	}

	cb2 := manager.GetCircuitBreaker("test-service")
	if cb2 == nil {
		t.Fatal("Expected to retrieve existing CircuitBreaker")
	}

	if cb1 != cb2 {
		t.Error("Expected same instance for same circuit name")
	}

	cb3 := manager.GetCircuitBreaker("nonexistent")
	if cb3 != nil {
		t.Error("Expected nil for nonexistent circuit")
	}
}
