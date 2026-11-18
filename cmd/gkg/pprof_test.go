package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

func TestServeRejectsNonLoopbackPprof(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  pprof_addr: 0.0.0.0:6060\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", path)
	t.Setenv("GKG_MEMGRAPH_URI", "invalid://unused")
	for _, addr := range []string{":6060", "192.0.2.1:6060", "[::]:6060", "example.invalid:6060"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("GKG_SERVER_PPROF_ADDR", addr)
			var out, stderr bytes.Buffer
			if code := runServe(nil, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "pprof") || !strings.Contains(stderr.String(), "loopback") {
				t.Fatalf("serve exit = %d, stderr = %q, want a pprof loopback error", code, stderr.String())
			}
		})
	}
}

func TestMainMuxDoesNotServePprof(t *testing.T) {
	cfg := config.Config{}
	h := newAPIServer(cfg, nil, nil, nil, obs.New()).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/", nil))
	if rec.Code != 404 {
		t.Errorf("main mux /debug/pprof/ status = %d, want 404", rec.Code)
	}
}

func TestPprofConfigEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  pprof_addr: 'localhost:0'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_SERVER_PPROF_ADDR", "127.0.0.1:0")
	cfg, err := config.Load(path)
	if err != nil || cfg.Server.PprofAddr != "127.0.0.1:0" {
		t.Fatalf("config with env override = %q, %v", cfg.Server.PprofAddr, err)
	}
}

func TestPprofUsesSeparateLoopbackListener(t *testing.T) {
	if server, listener, err := listenPprof(""); server != nil || listener != nil || err != nil {
		t.Fatalf("empty pprof address = %v, %v, %v", server, listener, err)
	}
	server, ln, err := listenPprof("localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("pprof listener IP = %s, want 127.0.0.1", ip)
	}
	rec := httptest.NewRecorder()
	server.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/goroutine", nil))
	if rec.Code != 200 {
		t.Errorf("pprof listener /debug/pprof/goroutine status = %d, want 200", rec.Code)
	}
}

func TestPprofSymbolAcceptsGetAndPost(t *testing.T) {
	server, ln, err := listenPprof("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not get a program counter for symbol lookup")
	}
	address := fmt.Sprintf("%#x", pc)
	const path = "/debug/pprof/symbol"
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, path+"?"+address, nil),
		httptest.NewRequest(http.MethodPost, path, strings.NewReader(address)),
	} {
		rec := httptest.NewRecorder()
		server.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), address+" "+runtime.FuncForPC(pc).Name()+"\n") {
			t.Errorf("%s %s status = %d, body = %q, want 200 and the resolved symbol", req.Method, path, rec.Code, rec.Body.String())
		}
	}
}
