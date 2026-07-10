package internal_test

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/diegoyosiura/circuit-breaker/internal"
)

// Porta efêmera (0): sem colisão com processos externos, sem sleep — Start()
// retorna com o listener pronto.
func TestFakeServer_HandlesRequests(t *testing.T) {
	fs := internal.NewFakeServer("127.0.0.1", 0)
	fs.SetDelay(10*time.Millisecond, 30*time.Millisecond)
	if err := fs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(fs.Close)

	resp, err := http.Get(fmt.Sprintf("http://%s/teste", fs.Addr()))
	if err != nil {
		t.Fatalf("Server did not respond: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
}

// Vários servidores simultâneos, cada um em sua porta efêmera distinta.
func TestFakeServer_MultipleServers(t *testing.T) {
	const n = 3
	addrs := make(map[string]bool, n)
	for i := range n {
		fs := internal.NewFakeServer("127.0.0.1", 0)
		fs.SetDelay(time.Millisecond, time.Millisecond)
		if err := fs.Start(); err != nil {
			t.Fatalf("Start server %d: %v", i, err)
		}
		t.Cleanup(fs.Close)

		addr := fs.Addr()
		if addr == "" || addrs[addr] {
			t.Fatalf("server %d: endereço inválido ou duplicado %q", i, addr)
		}
		addrs[addr] = true

		resp, err := http.Get(fmt.Sprintf("http://%s/teste", addr))
		if err != nil {
			t.Fatalf("server %d did not respond: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("server %d: expected 200, got %d", i, resp.StatusCode)
		}
	}
}

// A implementação antiga engolia o erro de bind e o teste consultava o
// processo dono da porta (o 404 flaky). Agora o erro chega ao chamador.
func TestFakeServer_PortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("helper listener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	port := l.Addr().(*net.TCPAddr).Port
	fs := internal.NewFakeServer("127.0.0.1", port)
	t.Cleanup(fs.Close)

	if err := fs.Start(); err == nil {
		t.Fatal("expected bind error for occupied port, got nil")
	}
}
