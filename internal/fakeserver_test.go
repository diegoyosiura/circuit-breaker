package internal_test

import (
	"github.com/diegoyosiura/circuit-breaker/internal"
	"net/http"
	"testing"
	"time"
)

func TestFakeServer_HandlesRequests(t *testing.T) {
	fs := internal.NewFakeServer("localhost", 8081)
	go fs.Listen()
	time.Sleep(2 * time.Second) // give server time to start

	req, err := http.NewRequest("GET", "http://localhost:8081/teste", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Server did not respond: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}
