package api

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// TestUIServesEmbeddedAssets pins the /ui/ mount: the index page, the
// static assets, and a 404 for unknown paths.
func TestUIServesEmbeddedAssets(t *testing.T) {
	h := apiHandler(t, config.Config{}, nil, nil)

	rr := do(t, h, http.MethodGet, "/ui/", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /ui/: code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /ui/: Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(rr.Body.String(), `id="graph"`) {
		t.Errorf("GET /ui/: the body misses the graph canvas marker")
	}

	rr = do(t, h, http.MethodGet, "/ui/app.js", nil, nil)
	if rr.Code != http.StatusOK {
		t.Errorf("GET /ui/app.js: code = %d, want 200", rr.Code)
	}

	rr = do(t, h, http.MethodGet, "/ui/nope.js", nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /ui/nope.js: code = %d, want 404", rr.Code)
	}
}

// TestUIRedirectsBarePath pins GET /ui → /ui/.
func TestUIRedirectsBarePath(t *testing.T) {
	h := apiHandler(t, config.Config{}, nil, nil)
	rr := do(t, h, http.MethodGet, "/ui", nil, nil)
	if rr.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /ui: code = %d, want 301", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("GET /ui: Location = %q, want /ui/", loc)
	}
}

// TestUIBearerPolicy pins the split the UI needs: the static shell is
// reachable without the key — a browser navigation cannot attach an
// Authorization header, and the page must load to ask for the key it then
// sends on its fetches — while every data path stays guarded.
func TestUIBearerPolicy(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.APIKey = "sekret"
	h := NewServer(cfg, nil, nil).Handler()

	shell := []struct {
		path string
		want int
	}{
		{"/ui", http.StatusMovedPermanently},
		{"/ui/", http.StatusOK},
		{"/ui/index.html", http.StatusMovedPermanently},
		{"/ui/app.css", http.StatusOK},
		{"/ui/app.js", http.StatusOK},
	}
	for _, tc := range shell {
		rr := do(t, h, http.MethodGet, tc.path, nil, nil)
		if rr.Code != tc.want {
			t.Errorf("GET %s without a key: code = %d, want %d (the shell must load so the page can ask for the key)", tc.path, rr.Code, tc.want)
		}
	}

	// Anything else under /ui is not exempt.
	rr := do(t, h, http.MethodGet, "/ui/nope.js", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /ui/nope.js without a key: code = %d, want 401", rr.Code)
	}
	rr = do(t, h, http.MethodGet, "/ui/nope.js", nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET /ui/nope.js with a key: code = %d, want 404", rr.Code)
	}

	// Data paths stay behind the key.
	rr = do(t, h, http.MethodGet, "/v1/documents", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/documents without a key: code = %d, want 401", rr.Code)
	}
	rr = do(t, h, http.MethodGet, "/v1/documents", nil, map[string]string{"Authorization": "Bearer sekret"})
	if rr.Code == http.StatusUnauthorized {
		t.Errorf("GET /v1/documents with a key: got 401, want the request served")
	}
}

// Exercise the served answer/stream functions with a small DOM fixture;
// the server renderer's security policy is covered by the Markdown tests.
func TestUIAnswerHTMLAndStreamReplacement(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("GKG_REQUIRE_NODE") == "1" {
			t.Fatal("node is required for the UI behavior check")
		}
		t.Skip("node is required for the UI behavior check")
	}
	rr := do(t, apiHandler(t, config.Config{}, nil, nil), http.MethodGet, "/ui/app.js", nil, nil)
	app := rr.Body.String()
	start := strings.Index(app, "  function insertMarkdown(")
	end := strings.Index(app, "  async function ask()")
	if start < 0 || end <= start {
		t.Fatal("missing answer rendering functions")
	}
	cmd := exec.Command(node, "-e", `
const assert = require("node:assert/strict");
const vm = require("node:vm");
function el(tag, className, text) {
 return {
  tag, className, textContent: text || "", children: [], links: [],
  append(...children) { this.children.push(...children); },
  replaceChildren(...children) { this.children = children; },
  set innerHTML(html) {
   this.html = html;
   this.links = [...html.matchAll(/href="([^"]*)"/g)].map(m => ({getAttribute() { return m[1]; }}));
  },
  querySelectorAll() { return this.links; },
 };
}
const nodes = {"ask-answer":el("div"), "ask-evidence":el("ul"), "ask-status":el("span")};
const context = {el, $:id => nodes[id], apiKey:"", TextDecoder};
vm.createContext(context);
vm.runInContext(process.argv[1] + "\nthis.render = renderAnswer; this.stream = askViaStream;", context);
const html = '<h1>Answer</h1><table><tr><td>value</td></tr></table><a href="https://example.com">link</a><a href="#sec">local</a>';
context.render({answer:"# Answer", answer_html:html});
let body = nodes["ask-answer"].children[0];
assert.equal(body.className, "markdown-body");
assert.equal(body.html, html);
assert.equal(body.links[0].target, "_blank");
assert.equal(body.links[0].rel, "noopener noreferrer");
assert.equal(body.links[1].target, undefined);
context.render({answer:"<plain>"});
body = nodes["ask-answer"].children[0];
assert.equal(body.textContent, "<plain>");
assert.equal(body.html, undefined);
let reads = 0;
const frame = (name, data) => new TextEncoder().encode("event: " + name + "\ndata: " + JSON.stringify(data) + "\n\n");
context.fetch = async () => ({ok:true,status:200,body:{getReader() { return {async read() {
 reads++;
 if (reads === 1) return {value:frame("delta", {text:"# Progress"})};
 if (reads === 2) {
  assert.equal(nodes["ask-answer"].children[0].textContent, "# Progress");
  assert.equal(nodes["ask-answer"].children[0].html, undefined);
  return {value:frame("done", {answer:"# Answer", answer_html:html})};
 }
 // The done frame must replace the progressive text before EOF.
 assert.equal(nodes["ask-answer"].children[0].html, html);
 return {done:true};
}}; }}});
context.stream("question", nodes["ask-status"]).catch(err => { console.error(err); process.exitCode = 1; });
`, app[start:end])
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("answer UI: %v\n%s", err, out)
	}
	rr = do(t, apiHandler(t, config.Config{}, nil, nil), http.MethodGet, "/ui/", nil, nil)
	if !strings.Contains(rr.Body.String(), `id="reader-body" class="markdown-body"`) || !strings.Contains(app, "insertMarkdown(body, html);") {
		t.Error("reader and answers must share Markdown insertion and styling")
	}
}
