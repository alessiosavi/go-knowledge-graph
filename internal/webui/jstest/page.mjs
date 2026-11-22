import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { setImmediate } from "node:timers/promises";
import vm from "node:vm";

const script = readFileSync(new URL("../assets/app.js", import.meta.url), "utf8");
export const html = readFileSync(new URL("../assets/index.html", import.meta.url), "utf8");
export const keyStore = "gkg.api.key";
export const savedKey = "synthetic-saved-key";

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
  removeAttribute(name) {
    delete this[name];
  }
  setAttribute(name, value) {
    this[name] = value;
  }
}

export async function page(key = savedKey, agentStatus = { enabled: true, stream: true }) {
  const elements = new Map();
  for (const match of html.matchAll(/<[^>]+\bid="([^"]+)"[^>]*>/g)) {
    elements.set(match[1], new Element(/\bhidden\b/.test(match[0])));
  }
  const storage = new Map(key ? [[keyStore, key]] : []);
  const requests = [];
  const replies = new Map([["/v1/agent/status", agentStatus]]);
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
    Blob, URL, TextDecoder, Option: class extends Element {
      constructor(text, value) { super(); this.textContent = text; this.value = value; }
    },
    FormData,
    fetch: async (path, options) => {
      requests.push({ path, ...options });
      const reply = replies.get(path);
      if (typeof reply === "function") return reply();
      const status = path === unauthorizedPath ? 401 : 200;
      return {
        status,
        ok: status === 200,
        headers: { get: (name) => name.toLowerCase() === "www-authenticate" ? challenge : null },
        json: async () => reply ?? {},
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
