package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

func listenPprof(addr string) (*http.Server, net.Listener, error) {
	if addr == "" {
		return nil, nil, nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("pprof address %q must be a loopback host:port: %w", addr, err)
	}
	if host == "localhost" {
		host = "127.0.0.1" // Pin localhost to loopback without a name lookup.
	}
	if !net.ParseIP(host).IsLoopback() {
		return nil, nil, fmt.Errorf("pprof address %q must use a loopback host", addr)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, nil, fmt.Errorf("pprof listen on %q: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}, ln, nil
}
