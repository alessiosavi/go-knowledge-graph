package main

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProbeCommandCA(t *testing.T) {
	for _, tls := range []bool{false, true} {
		name := "http"
		if tls {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			var server *httptest.Server
			dir := t.TempDir()
			if tls {
				server = httptest.NewTLSServer(handler)
				write(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			p := &probe{dir: dir, base: server.URL}
			cmd, _, _ := p.command("GET", "/healthz", "", 5)
			found := false
			for i, arg := range cmd.Args {
				if arg == "--cacert" {
					found = true
					if i+1 >= len(cmd.Args) || cmd.Args[i+1] != filepath.Join(dir, "ca.crt") {
						t.Fatal("HTTPS must use the private CA file")
					}
				}
			}
			if found != tls {
				t.Fatalf("--cacert present=%t for %s base", found, name)
			}
			if r := p.request("GET", "/healthz", "", nil); r.code != http.StatusOK {
				t.Fatalf("probe returned HTTP %d", r.code)
			}
		})
	}
}

func TestPasswordRotationProbe(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "edgecheck")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v: %s", err, out)
	}
	for _, tc := range []struct {
		name             string
		oldCode, newCode int
		wantOK           bool
	}{
		{"rotated", 401, 200, true},
		{"old password still accepted", 200, 200, false},
		{"new password rejected", 401, 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var oldSeen, newSeen atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				login, password, ok := r.BasicAuth()
				if !ok || login != "edgecheck" || r.Method != "GET" || r.URL.Path != "/v1/documents" {
					t.Error("rotation must authenticate the same login on a protected route")
					w.WriteHeader(400)
					return
				}
				switch password {
				case "old-password":
					oldSeen.Add(1)
					w.WriteHeader(tc.oldCode)
				case "new-password":
					newSeen.Add(1)
					w.WriteHeader(tc.newCode)
				default:
					t.Error("unexpected rotation credential")
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			write(filepath.Join(dir, "old.curl"), []byte("user = \"edgecheck:old-password\"\n"))
			write(filepath.Join(dir, "auth.curl"), []byte("user = \"edgecheck:new-password\"\n"))
			out, err := exec.Command(bin, "rotation", dir, server.URL).CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("rotation success=%t, want %t: %s", err == nil, tc.wantOK, out)
			}
			if oldSeen.Load() != 1 || tc.oldCode == 401 && newSeen.Load() != 1 {
				t.Fatalf("rotation did not check both credential generations: old=%d new=%d", oldSeen.Load(), newSeen.Load())
			}
		})
	}
}

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
		if strings.Contains(strings.ToLower(r.URL.RawPath), "%2e%2e") {
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

func TestEncodedPathsUpstreamProof(t *testing.T) {
	record := func(method, path string) []byte {
		return append(encode(map[string]string{"msg": "http", "method": method, "path": path}), '\n')
	}
	control := record("GET", "/v1/documents")
	before := append([]byte{}, control...)
	for _, pair := range encodedPathProbes {
		path, err := url.PathUnescape(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		before = append(before, record(pair[0], path)...)
	}
	after := append(append([]byte{}, before...), control...)
	if err := encodedPathsUpstreamProof(before, after); err != nil {
		t.Fatal("historical requests must not invalidate a fresh log proof:", err)
	}
	for _, invalid := range [][]byte{before, control} {
		if encodedPathsUpstreamProof(before, invalid) == nil {
			t.Error("missing positive control or truncated logs accepted")
		}
	}
	for _, pair := range encodedPathProbes {
		decoded, _ := url.PathUnescape(pair[1])
		for _, path := range []string{pair[1], decoded} {
			leak := append(append([]byte{}, after...), record(pair[0], path)...)
			if encodedPathsUpstreamProof(before, leak) == nil {
				t.Errorf("missed upstream leak: %s %s", pair[0], path)
			}
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
