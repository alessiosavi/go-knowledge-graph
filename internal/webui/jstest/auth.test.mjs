import assert from "node:assert/strict";
import { resolveObjectURL } from "node:buffer";
import { setImmediate } from "node:timers/promises";
import test from "node:test";
import { html, keyStore, page, savedKey } from "./page.mjs";

for (const stream of [false, true]) {
  for (const basic of [true, false]) {
    test(`${stream ? "stream" : "REST"} 401 ${basic ? "with Basic challenge clears stale bearer" : "without challenge prompts for bearer"}`, async () => {
      const p = await page();
      const path = stream ? "/v1/agent/ask/stream" : "/v1/documents";
      p.reject(path, basic ? 'Basic realm="gkg"' : null);
      p.elements.get("ask-input").value = "synthetic question";
      const before = p.requests.length;
      await p.fire(stream ? "ask-form" : "docs-refresh", stream ? "submit" : "click");
      assert.equal(p.requests[before].headers.Authorization, `Bearer ${savedKey}`);
      assert.equal(p.elements.get("auth-bar").hidden, basic);
      if (basic) {
        assert.equal(p.storage.has(keyStore), false, "stale key must be removed");
        assert.match(p.elements.get("status").textContent, /reload.*sign in/i);
        assert.equal(p.elements.get("auth-forget").hidden, true);
      } else {
        assert.equal(p.storage.get(keyStore), savedKey);
        assert.match(p.elements.get("status").textContent, /API key required/);
      }
      assert.equal(p.requests.some((r) => r.path === "/v1/agent/ask"), false, "401 must not fall back to another ask");
      await p.fire("docs-refresh", "click");
      assert.equal(p.requests.at(-1).headers.Authorization, basic ? undefined : `Bearer ${savedKey}`);
    });
  }
}

test("forget saved key removes storage and bearer and hides its button", async () => {
  const p = await page();
  assert.ok(p.elements.has("auth-forget"), "missing #auth-forget");
  assert.equal(p.elements.get("auth-forget").hidden, false);
  await p.fire("auth-forget", "click");
  assert.equal(p.storage.has(keyStore), false);
  assert.equal(p.elements.get("auth-forget").hidden, true);
  await p.fire("docs-refresh", "click");
  assert.equal(p.requests.at(-1).headers.Authorization, undefined);
});

test("fresh page sends no bearer and saving a key reveals the forget button", async () => {
  const p = await page("");
  assert.ok(p.requests.length >= 2, "startup probes must run");
  for (const request of p.requests) assert.equal(request.headers.Authorization, undefined);
  assert.ok(p.elements.has("auth-forget"), "missing #auth-forget");
  assert.equal(p.elements.get("auth-forget").hidden, true);
  p.elements.get("auth-key").value = savedKey;
  await p.fire("auth-save", "click");
  assert.equal(p.storage.get(keyStore), savedKey);
  assert.equal(p.elements.get("auth-forget").hidden, false);
  assert.equal(p.requests.at(-1).headers.Authorization, `Bearer ${savedKey}`);
});

async function selectEntity(p) {
  p.replies.set("/v1/search", { hits: [{ kind: "entity", id: "zorbo", title: "Zorbo" }] });
  p.replies.set("/v1/entities/zorbo", { id: "zorbo", name: "Zorbo", hidden: { unclear: 1, unjudged: 2, rejected: 1 } });
  p.replies.set("/v1/entities/zorbo?include_low_confidence=true", { id: "zorbo", name: "Zorbo" });
  p.elements.get("search-input").value = "Zorbo";
  await p.fire("search-form", "submit");
  p.elements.get("results").children[0].dispatch("click");
  await setImmediate();
}

test("entity and neighborhood requests omit low confidence by default", async () => {
  const p = await page();
  await selectEntity(p);
  const entity = p.requests.find((r) => r.path.startsWith("/v1/entities/"));
  assert.equal(entity.path, "/v1/entities/zorbo");
  const traversal = p.requests.find((r) => r.path === "/v1/traverse");
  assert.equal(Object.hasOwn(JSON.parse(traversal.body), "include_low_confidence"), false);
});

test("low confidence checkbox reloads entity and neighborhood with opt-in", async () => {
  const p = await page();
  await selectEntity(p);
  const checkbox = p.elements.get("include-low-confidence");
  assert.ok(checkbox, "missing low-confidence checkbox");
  assert.equal(checkbox.checked, false);
  checkbox.checked = true;
  await p.fire("include-low-confidence", "change");
  assert.ok(p.requests.some((r) => r.path === "/v1/entities/zorbo?include_low_confidence=true"));
  assert.equal(JSON.parse(p.requests.filter((r) => r.path === "/v1/traverse").at(-1).body).include_low_confidence, true);
  checkbox.checked = false;
  await p.fire("include-low-confidence", "change");
  assert.equal(p.requests.filter((r) => r.path.startsWith("/v1/entities/")).at(-1).path, "/v1/entities/zorbo");
  assert.equal(Object.hasOwn(JSON.parse(p.requests.at(-1).body), "include_low_confidence"), false);
});

test("entity card explains hidden facts by state", async () => {
  const p = await page();
  await selectEntity(p);
  assert.ok(p.elements.get("entity").children.some((e) => e.textContent === "Hidden: 1 unclear, 2 unjudged, 1 rejected facts"));
});

test("domain keys are hidden like document keys", async () => {
 const p=await page();
 p.replies.set("/v1/search",{hits:[{kind:"entity",id:"acme",title:"Acme"}]});
 p.replies.set("/v1/entities/acme",{id:"acme",name:"Acme",key:"@a:acme"});
 p.elements.get("search-input").value="Acme";
 await p.fire("search-form","submit");
 p.elements.get("results").children[0].dispatch("click");
 await setImmediate();
 const texts=(e)=>[e.textContent,...e.children.flatMap(texts)].join(" ");
 assert.equal(texts(p.elements.get("entity" )).includes("@a:acme"),false);
});

test("domain pickers scope search, JSON ask and streaming ask", async () => {
  const p = await page();
  assert.ok(p.elements.has("search-domain"));
  assert.ok(p.elements.has("ask-domain"));
  assert.ok(p.requests.some((r) => r.path === "/v1/domains"));
  p.elements.get("search-domain").value = "a";
  p.elements.get("search-input").value = "Quillon";
  await p.fire("search-form", "submit");
  assert.equal(JSON.parse(p.requests.find((r) => r.path === "/v1/search").body).domain, "a");
  p.elements.get("ask-domain").value = "b";
  p.elements.get("ask-input").value = "Quillon?";
  await p.fire("ask-form", "submit");
  const asks = p.requests.filter((r) => r.path.startsWith("/v1/agent/ask"));
  assert.ok(asks.some((r) => r.path.endsWith("/stream")));
  assert.ok(asks.some((r) => r.path === "/v1/agent/ask"));
  for (const req of asks) assert.equal(JSON.parse(req.body).domain, "b");
});

test("ask renders find_products titles and graph IDs alongside search evidence", async () => {
  const p = await page(savedKey, { enabled: true, stream: false });
  p.elements.get("ask-input").value = "Find a product";
  p.replies.set("/v1/agent/ask", { evidence: [
    { tool: "find_products", result: { hits: [{ product: { id: "p-1", title: "Blue Widget" } }] } },
    { tool: "search", result: { hits: [{ kind: "entity", id: "e-1", title: "Widget Maker", snippet: "A manufacturer" }] } },
  ] });
  await p.fire("ask-form", "submit");
  const [products, search] = p.elements.get("ask-evidence").children;
  const texts = (element) => [element.textContent, ...element.children.flatMap(texts)].join(" ");
  assert.match(texts(products), /Blue Widget.*p-1/);
  assert.equal(search.children[1].children[0].textContent, "entity");
  assert.equal(search.children[1].children[1].textContent, "Widget Maker");
  assert.equal(search.children[1].children[2].textContent, "A manufacturer");
});

test("ask previews get_product and list_features as JSON", async () => {
  const p = await page(savedKey, { enabled: true, stream: false });
  p.elements.get("ask-input").value = "Show product details and feature IDs";
  const detail = { id: "p-1", title: "Blue Widget", features: [], features_omitted: 2 };
  const listing = { active_release_id: "r1", features: [{ id: "color", stores: ["example.test"] }] };
  p.replies.set("/v1/agent/ask", { evidence: [
    { tool: "get_product", result: detail }, { tool: "list_features", result: listing },
  ] });
  await p.fire("ask-form", "submit");
  const [product, features] = p.elements.get("ask-evidence").children;
  assert.equal(product.children[1].textContent, JSON.stringify(detail));
  assert.equal(features.children[1].textContent, JSON.stringify(listing));
});

const markdown = "# Résumé\n\n| Key | Value |\n| --- | --- |\n| α | β |\n\n```go\nfmt.Println(\"🌱\")\n```\n";
const capabilities = { top_k: true, reasoning_efforts: ["none", "high", "max"] };
const frame = (name, data) => "event: " + name + "\ndata: " + JSON.stringify(data) + "\n\n";

for (const stream of [false, true]) {
  test(`${stream ? "stream" : "JSON"} downloads exact final Markdown and clears it on the next ask`, async () => {
    const p = await page(savedKey, { enabled: true, stream, generation: capabilities });
    const get = (id) => p.elements.get(id);
    const download = get("ask-download");
    const path = stream ? "/v1/agent/ask/stream" : "/v1/agent/ask";
    assert.ok(download.hidden);
    assert.match(html, /id="ask-download" download="gkg-answer\.md"/);
    assert.deepEqual(get("ask-reasoning").children.map((option) => option.value), ["", "none", "high", "max"]);
    get("ask-input").value = "First?";
    get("ask-temperature").value = "0";
    get("ask-top-p").value = "0";
    get("ask-top-k").value = "8";
    get("ask-reasoning").value = "none";
    const complete = { answer: markdown };
    if (stream) {
      let finish;
      const body = new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(frame("delta", { text: "partial" })));
          finish = () => {
            controller.enqueue(new TextEncoder().encode(frame("done", complete)));
            controller.close();
          };
        },
      });
      p.replies.set(path, () => ({ ok: true, body }));
      await p.fire("ask-form", "submit");
      assert.ok(download.hidden);
      assert.equal(get("ask-answer").children[0].textContent, "partial");
      finish();
      await setImmediate();
    } else {
      p.replies.set(path, complete);
      await p.fire("ask-form", "submit");
    }
    assert.equal(download.hidden, false);
    const url = download.href;
    const before = p.requests.length;
    const file = resolveObjectURL(url);
    assert.equal(file.type, "text/markdown;charset=utf-8");
    assert.deepEqual(Buffer.from(await file.arrayBuffer()), Buffer.from(markdown));
    assert.equal(p.requests.length, before);
    get("ask-input").value = "Second?";
    p.replies.set(path, () => ({ ok: false, status: 400, json: async () => ({ detail: "invalid setting" }) }));
    await p.fire("ask-form", "submit");
    assert.ok(download.hidden);
    assert.equal(download.href, undefined);
    assert.equal(resolveObjectURL(url), undefined);
    assert.match(get("ask-status").textContent, /invalid setting/);
    const asks = p.requests.filter((r) => r.path.startsWith("/v1/agent/ask"));
    assert.equal(asks.length, 2);
    for (const ask of asks) {
      assert.deepEqual(JSON.parse(ask.body).generation, { temperature: 0, top_p: 0, top_k: 8, reasoning_effort: "none" });
    }
  });
}

test("uses page defaults and omits unavailable controls", async () => {
  const p = await page(savedKey, { enabled: true, stream: false, generation: { top_k: false, reasoning_efforts: [] } });
  const get = (id) => p.elements.get(id);
  assert.equal(get("ask-settings").hidden, false);
  assert.equal(get("ask-top-k").disabled, true);
  assert.equal(get("ask-top-k-note").hidden, false);
  assert.equal(get("ask-reasoning").disabled, true);
  get("ask-input").value = "Q";
  get("ask-top-k").value = "5";
  await p.fire("ask-form", "submit");
  assert.deepEqual(JSON.parse(p.requests.at(-1).body), { question: "Q", domain: "" });
});

test("stream transport fallback uses the settings captured at submission", async () => {
  const p = await page(savedKey, { enabled: true, stream: true, generation: capabilities });
  const get = (id) => p.elements.get(id);
  get("ask-input").value = "Q";
  get("ask-temperature").value = "0.8";
  p.replies.set("/v1/agent/ask/stream", () => {
    get("ask-temperature").value = "1.2";
    throw new TypeError("stream disconnected");
  });
  await p.fire("ask-form", "submit");
  const asks = p.requests.filter((r) => r.path.startsWith("/v1/agent/ask"));
  assert.equal(asks.length, 2);
  assert.equal(asks[0].body, asks[1].body);
  assert.deepEqual(JSON.parse(asks[1].body).generation, { temperature: 0.8 });
});

for (const kind of ["auth_permanent", "auth_transient"]) {
  test(`stream ${kind} is terminal and does not fall back to the JSON ask`, async () => {
    const p = await page(savedKey, { enabled: true, stream: true, generation: capabilities });
    p.elements.get("ask-input").value = "Q";
    p.replies.set("/v1/agent/ask/stream", () => ({
      ok: false, status: 502, statusText: "Bad Gateway",
      json: async () => ({ error: kind, detail: "credential failure" }),
    }));
    await p.fire("ask-form", "submit");
    const asks = p.requests.filter((r) => r.path.startsWith("/v1/agent/ask"));
    assert.equal(asks.length, 1);
  });
}
