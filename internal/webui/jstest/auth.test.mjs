import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { setImmediate } from "node:timers/promises";
import test from "node:test";
import vm from "node:vm";

const script = readFileSync(new URL("../assets/app.js", import.meta.url), "utf8");
const html = readFileSync(new URL("../assets/index.html", import.meta.url), "utf8");
const keyStore = "gkg.api.key";
const savedKey = "synthetic-saved-key";

class Element {
  constructor(hidden = false) {
    this.hidden = hidden;
    this.value = "";
    this.checked = false;
    this.textContent = "";
    this.dataset = {};
    this.listeners = new Map();
    this.children = [];
    this.classList = { add() {} };
  }
  addEventListener(name, listener) {
    this.listeners.set(name, listener);
  }
  dispatch(name) {
    assert.ok(this.listeners.has(name), `missing ${name} listener`);
    this.listeners.get(name)({ preventDefault() {}, stopPropagation() {} });
  }
  append(...children) {
    this.children.push(...children);
  }
  replaceChildren(...children) {
    this.children = children;
  }
  querySelector(selector) {
    assert.equal(selector, "tbody");
    return this.tbody ??= new Element();
  }
  setAttribute(name, value) {
    this[name] = value;
  }
}

async function page(key = savedKey) {
  const elements = new Map();
  for (const match of html.matchAll(/<[^>]+\bid="([^"]+)"[^>]*>/g)) {
    elements.set(match[1], new Element(/\bhidden\b/.test(match[0])));
  }
  const storage = new Map(key ? [[keyStore, key]] : []);
  const requests = [];
  const replies = new Map();
  let unauthorizedPath = "";
  let challenge = "";
  vm.runInNewContext(script, {
    document: {
      readyState: "complete",
      getElementById: (id) => elements.get(id),
      createElement: () => new Element(),
      createElementNS: () => new Element(),
      createTextNode: (text) => Object.assign(new Element(), { textContent: text }),
    },
    localStorage: {
      getItem: (name) => storage.get(name) ?? null,
      setItem: (name, value) => storage.set(name, value),
      removeItem: (name) => storage.delete(name),
    },
    FormData: class {},
    fetch: async (path, options) => {
      requests.push({ path, ...options });
      const status = path === unauthorizedPath ? 401 : 200;
      return {
        status,
        ok: status === 200,
        headers: { get: (name) => name.toLowerCase() === "www-authenticate" ? challenge : null },
        json: async () => replies.get(path) ?? (path === "/v1/agent/status" ? { enabled: true, stream: true } : {}),
      };
    },
  });
  await setImmediate();
  return {
    elements, storage, requests, replies,
    reject(path, header) {
      unauthorizedPath = path;
      challenge = header;
    },
    async fire(id, event) {
      assert.ok(elements.has(id), `missing #${id}`);
      elements.get(id).dispatch(event);
      await setImmediate();
    },
  };
}

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
