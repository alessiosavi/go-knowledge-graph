package apidocs

import (
	"io/fs"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// These are the complete publicly served files; adding one requires updating
// the exact bearer exemptions and their API-level tests.
func TestAssetsFileSet(t *testing.T) {
	entries, err := fs.ReadDir(assets, "assets")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"index.html", "swagger-initializer.js", "swagger-ui-bundle.js", "swagger-ui.css"}
	if !slices.Equal(names, want) {
		t.Fatalf("embedded assets=%v, want %v; update bearer policy when adding files", names, want)
	}
}

func TestPageLoadsOnlyLocalAssetsWithoutInlineScripts(t *testing.T) {
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	root, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	var scripts, styles []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "link") {
			src := ""
			for _, a := range n.Attr {
				if a.Key == "src" || a.Key == "href" {
					src = a.Val
				}
			}
			if !strings.HasPrefix(src, "./") {
				t.Errorf("%s source must be local: %q", n.Data, src)
			}
			if n.Data == "script" {
				scripts = append(scripts, src)
				if n.FirstChild != nil {
					t.Error("inline script violates the CSP")
				}
			} else {
				styles = append(styles, src)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	if !slices.Equal(scripts, []string{"./swagger-ui-bundle.js", "./swagger-initializer.js"}) {
		t.Errorf("scripts=%v", scripts)
	}
	if !slices.Equal(styles, []string{"./swagger-ui.css"}) {
		t.Errorf("styles=%v", styles)
	}
}

func TestInitializerDisablesRemoteValidator(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node required for Swagger initializer check")
	}
	data, err := assets.ReadFile("assets/swagger-initializer.js")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", `
const assert = require("node:assert/strict");
const vm = require("node:vm");
let calls = 0;
const SwaggerUIBundle = (config) => {
 calls++;
 assert.equal(config.url, "./openapi.yaml");
 assert.equal(config.validatorUrl, null);
 assert.equal(config.deepLinking, true);
 assert.equal(config.dom_id, "#swagger-ui");
 return {config};
};
SwaggerUIBundle.presets = {apis:{}};
const context = {SwaggerUIBundle, window:{}};
vm.runInNewContext(process.argv[1], context);
assert.equal(calls, 1);
assert.ok(context.window.ui);
`, string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initializer: %v\n%s", err, out)
	}
}
