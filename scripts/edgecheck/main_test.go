package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestJavaQueryProbeOutsideExcludedBody(t *testing.T) {
	var seen atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.URL.Query().Get("edgecheck_java_query"), "java.lang.Runtime") {
			if r.Method != "POST" || r.URL.Path != "/v1/ingest" {
				t.Error("query probe must exercise a route with a body exclusion")
			}
			var document struct{ Text string }
			if json.Unmarshal(body, &document) != nil || document.Text != "synthetic" || strings.Contains(string(body), "java.lang.Runtime") {
				t.Error("Java query proof requires a benign body")
			}
			seen.Add(1)
		}
		if strings.Contains(r.URL.RawPath, "%2e%2e") {
			w.WriteHeader(400)
		} else {
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	write(filepath.Join(dir, "auth.curl"), nil)
	write(filepath.Join(dir, "attack-marker"), []byte("synthetic-marker"))
	p := &probe{dir: dir, base: server.URL}
	p.attacks("On")
	if seen.Load() != 1 {
		t.Fatal("attack battery must include one Java query probe")
	}
}

func TestClosedRouteResponses(t *testing.T) {
	for _, mode := range []string{"DetectionOnly", "On"} {
		for _, code := range []int{200, 401, 403, 404, 405, 500} {
			want := code == 404 || code == 405 || mode == "On" && code == 403
			if closedRouteResponse(mode, response{code: code}) != want {
				t.Errorf("%s HTTP %d: wrong closed-route verdict", mode, code)
			}
		}
	}
	if closedRouteResponse("On", response{code: 403, headers: http.Header{"Content-Type": {"application/json"}}}) {
		t.Fatal("a gkg JSON response is not an edge rejection")
	}
}

func TestMCPNearCap(t *testing.T) {
	var initializeBytes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			http.NewResponseController(w).Flush()
			<-r.Context().Done()
			return
		}
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		body, _ := io.ReadAll(r.Body)
		call := object(body)
		switch call["method"] {
		case "initialize":
			initializeBytes.Store(int64(len(body)))
			w.Header().Set("Mcp-Session-Id", "fixture")
			fmt.Fprint(w, `{"result":{}}`)
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			fmt.Fprint(w, `{"result":{"tools":[{},{},{},{},{},{},{}]}}`)
		default:
			fmt.Fprint(w, `{"result":{}}`)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	write(filepath.Join(dir, "auth.curl"), nil)
	p := &probe{dir: dir, base: server.URL}
	p.mcp(false)
	if n := initializeBytes.Load(); n != (4<<20)-1024 {
		t.Fatalf("MCP boundary was not exercised: %d bytes", n)
	}
}

func TestNoFilesUpstreamProof(t *testing.T) {
	positive := []byte("{\"msg\":\"http\",\"method\":\"GET\",\"path\":\"/v1/documents\"}\n")
	ingest := []byte("{\"msg\":\"http\",\"method\":\"POST\",\"path\":\"/v1/ingest\"}\n")
	before := append(append([]byte{}, positive...), ingest...)
	if err := noFilesUpstreamProof(before, append(append([]byte{}, before...), positive...)); err != nil {
		t.Fatal("historical requests do not invalidate a fresh positive/negative proof")
	}
	for _, after := range [][]byte{before, append(append(append([]byte{}, before...), positive...), ingest...), positive} {
		if noFilesUpstreamProof(before, after) == nil {
			t.Fatal("missing positive control, new upstream ingest or truncated log accepted")
		}
	}
}

func TestProbeBodyBoundaries(t *testing.T) {
	var jsonSizes, fileSizes []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			fmt.Fprint(w, `{"status":"done","deleted":1}`)
			return
		}
		if r.Header.Get("Content-Type") == "application/json" {
			body, _ := io.ReadAll(r.Body)
			if !json.Valid(body) {
				http.Error(w, "invalid JSON", 400)
				return
			}
			n := int64(len(body))
			jsonSizes = append(jsonSizes, n)
			if n > 4259840 {
				w.WriteHeader(413)
				fmt.Fprint(w, "edge body limit")
				return
			}
		} else {
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				return
			}
			part, err := reader.NextPart()
			if err != nil || part.FormName() != "file" {
				t.Error("missing file boundary")
				return
			}
			n, _ := io.Copy(io.Discard, part)
			fileSizes = append(fileSizes, n)
			if r.ContentLength > 21037056 {
				w.WriteHeader(413)
				fmt.Fprint(w, "nginx body limit")
				return
			}
			if r.ContentLength > 20<<20 {
				w.WriteHeader(413)
				fmt.Fprint(w, `{"error":"body_too_large"}`)
				return
			}
		}
		w.WriteHeader(202)
		fmt.Fprint(w, `{"document_id":"boundary"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	write(filepath.Join(dir, "auth.curl"), nil)
	p := &probe{dir: dir, base: server.URL}
	defer func() {
		if failure := recover(); failure != nil {
			t.Fatalf("boundary probe failed: %v", failure)
		}
	}()
	p.limits()
	p.rejectJSON()
	if fmt.Sprint(jsonSizes) != "[4193280 4259841]" || fmt.Sprint(fileSizes) != "[20970496 20972544 21037057]" {
		t.Fatalf("wrong probe sizes: JSON %v, files %v", jsonSizes, fileSizes)
	}
}

func TestRPCResultDistinguishesSchemaFromErrors(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"inputSchema":{"properties":{"error":{"type":"string"}}}}]}}`,
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[]}}\n\n",
	} {
		if _, err := rpcResult([]byte(body)); err != nil {
			t.Fatal("a tool schema's error property is not an RPC failure")
		}
	}
	for _, body := range []string{`{"error":{"code":-1}}`, `{"result":{"isError":true}}`, `not JSON`} {
		if _, err := rpcResult([]byte(body)); err == nil {
			t.Error("failed/malformed RPC accepted")
		}
	}
}
