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
}

async function page(key = savedKey) {
  const elements = new Map();
  for (const match of html.matchAll(/<[^>]+\bid="([^"]+)"[^>]*>/g)) {
    elements.set(match[1], new Element(/\bhidden\b/.test(match[0])));
  }
  const storage = new Map(key ? [[keyStore, key]] : []);
  const requests = [];
  let unauthorizedPath = "";
  let challenge = "";
  vm.runInNewContext(script, {
    document: {
      readyState: "complete",
      getElementById: (id) => elements.get(id),
      createElement: () => new Element(),
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
        json: async () => path === "/v1/agent/status" ? { enabled: true, stream: true } : {},
      };
    },
  });
  await setImmediate();
  return {
    elements, storage, requests,
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
