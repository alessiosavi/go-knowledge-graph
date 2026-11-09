// gkg web UI: a read-only viewer over the existing REST endpoints
// (/v1/search, /v1/entities, /v1/traverse, /v1/documents, /v1/ingest),
// plus the explicit ingest box. No dependencies; every request is
// same-origin. When the server asks for a bearer key (401), the page
// prompts for it once and keeps it in localStorage — never in URLs.
(() => {
  "use strict";

  const KEY_STORE = "gkg.api.key";
  const STATUS_POLL_MS = 1500;
  const STATUS_POLL_MAX = 80; // ~2 minutes
  const SVG_NS = "http://www.w3.org/2000/svg"; // XML namespace, not a network URL

  const $ = (id) => document.getElementById(id);

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = text;
    return node;
  }

  function svgEl(tag, attrs) {
    const node = document.createElementNS(SVG_NS, tag);
    for (const key of Object.keys(attrs || {}))
      node.setAttribute(key, attrs[key]);
    return node;
  }

  function fit(label, max) {
    const text = String(label == null ? "" : label);
    return text.length > max ? text.slice(0, max - 1) + "…" : text;
  }

  let apiKey = "";
  try {
    apiKey = localStorage.getItem(KEY_STORE) || "";
  } catch (_) {
    apiKey = "";
  }

  function showAuthBar() {
    $("auth-bar").hidden = false;
    $("status").textContent = "API key required";
  }

  // api talks to the gkg REST surface. A 401 reveals the key prompt once;
  // errors carry the server's detail when there is one.
  async function api(path, options) {
    const opts = options || {};
    const headers = Object.assign(
      {},
      opts.json ? { "Content-Type": "application/json" } : {},
      apiKey ? { Authorization: "Bearer " + apiKey } : {},
    );
    const res = await fetch(path, {
      method: opts.method || "GET",
      headers,
      body: opts.json ? JSON.stringify(opts.json) : undefined,
    });
    if (res.status === 401) {
      showAuthBar();
      throw new Error("unauthorized — provide the API key");
    }
    let data = null;
    try {
      data = await res.json();
    } catch (_) {
      data = null;
    }
    if (!res.ok) {
      throw new Error(
        (data && data.detail) || "HTTP " + res.status + " " + res.statusText,
      );
    }
    return data;
  }

  function fail(node, err) {
    node.replaceChildren(el("li", "error", String(err.message || err)));
  }

  function setBusy(button, busy, label) {
    if (!button) return;
    if (busy) {
      button.dataset.label = button.textContent;
      button.textContent = label || "…";
      button.disabled = true;
    } else {
      button.textContent = button.dataset.label || button.textContent;
      button.disabled = false;
    }
  }

  // ---- search -------------------------------------------------------------

  async function search(query) {
    const list = $("results");
    list.replaceChildren(el("li", "muted", "Searching…"));
    let data;
    try {
      data = await api("/v1/search", {
        method: "POST",
        json: { query: query, kind: "auto", limit: 20 },
      });
    } catch (err) {
      fail(list, err);
      return;
    }
    const hits = (data && data.hits) || [];
    list.replaceChildren();
    if (!hits.length) {
      list.append(el("li", "muted", "No hits."));
      return;
    }
    for (const hit of hits) {
      const item = el("li");
      const head = el("div");
      head.append(el("span", "badge", hit.kind || "hit"));
      head.append(document.createTextNode(hit.title || hit.id || ""));
      item.append(head);
      if (hit.snippet) item.append(el("div", "snippet", hit.snippet));
      if (hit.kind === "entity") {
        item.classList.add("hit");
        item.addEventListener("click", () => {
          loadEntity(hit.id).catch(() => {});
        });
      } else {
        item.append(el("div", "snippet", "doc " + (hit.id || "")));
      }
      list.append(item);
    }
  }

  // ---- entity card + facts ------------------------------------------------

  async function loadEntity(id) {
    const box = $("entity");
    box.replaceChildren(el("p", "muted", "Loading…"));
    const view = await api(
      "/v1/entities/" + encodeURIComponent(id) + "?include_low_confidence=true",
    );
    box.replaceChildren();
    const title = el("p");
    title.append(el("strong", null, view.name || view.id));
    if (view.type) title.append(el("span", "badge", view.type));
    box.append(title);
    if (view.key) box.append(el("p", "muted", view.key));
    if (view.aliases && view.aliases.length) {
      box.append(el("p", "muted", "aliases: " + view.aliases.join(", ")));
    }
    box.append(el("p", "muted", (view.mention_count || 0) + " mentions"));
    renderFacts(view.facts || []);
    await renderNeighborhood(view);
  }

  function renderFacts(facts) {
    const body = $("facts").querySelector("tbody");
    body.replaceChildren();
    if (!facts.length) {
      const row = el("tr");
      const cell = el("td", "muted", "No facts.");
      cell.colSpan = 4;
      row.append(cell);
      body.append(row);
      return;
    }
    for (const f of facts) {
      const row = el("tr");
      const fact = el("td");
      fact.append(el("span", "arrow", f.outgoing ? "→ " : "← "));
      fact.append(document.createTextNode(f.rel + " "));
      const other = f.other || {};
      const link = el("a", null, other.name || other.id || "?");
      link.href = "#";
      link.addEventListener("click", (ev) => {
        ev.preventDefault();
        loadEntity(other.id).catch(() => {});
      });
      fact.append(link);
      row.append(fact);
      row.append(el("td", null, Number(f.confidence || 0).toFixed(2)));
      row.append(el("td", "muted", f.judge || "—"));
      const evi = el("td", "muted");
      evi.append(el("div", null, f.excerpt || ""));
      if (f.doc_id)
        evi.append(
          el(
            "div",
            null,
            "doc " + f.doc_id + (f.chunk_id ? " · " + f.chunk_id : ""),
          ),
        );
      row.append(evi);
      body.append(row);
    }
  }

  // ---- neighborhood (one-hop radial SVG) ----------------------------------

  async function renderNeighborhood(entity) {
    const svg = $("graph");
    const placeholder = svgEl("text", {
      x: 320,
      y: 210,
      "text-anchor": "middle",
    });
    placeholder.textContent = "Loading…";
    svg.replaceChildren(placeholder);

    let sub;
    try {
      sub = await api("/v1/traverse", {
        method: "POST",
        json: {
          entity: entity.id,
          direction: "both",
          depth: 1,
          include_low_confidence: true,
          limit: 100,
        },
      });
    } catch (err) {
      placeholder.textContent = String(err.message || err);
      placeholder.setAttribute("class", "error");
      return;
    }

    const nodes = (sub && sub.nodes) || [];
    const edges = (sub && sub.edges) || [];
    const neighbors = nodes.filter((n) => n.id !== entity.id);
    svg.replaceChildren();
    if (!neighbors.length) {
      const none = svgEl("text", {
        x: 320,
        y: 210,
        "text-anchor": "middle",
        class: "muted",
      });
      none.textContent = "No fact edges from this entity yet.";
      svg.append(none);
      return;
    }

    const cx = 320,
      cy = 210,
      radius = 155;
    const pos = new Map();
    pos.set(entity.id, { x: cx, y: cy });
    neighbors.forEach((n, i) => {
      const angle = (2 * Math.PI * i) / neighbors.length - Math.PI / 2;
      pos.set(n.id, {
        x: cx + radius * Math.cos(angle),
        y: cy + radius * Math.sin(angle),
      });
    });

    for (const e of edges) {
      const a = pos.get(e.from),
        b = pos.get(e.to);
      if (!a || !b) continue;
      const g = svgEl("g", { class: "edge" });
      g.append(svgEl("line", { x1: a.x, y1: a.y, x2: b.x, y2: b.y }));
      const label = svgEl("text", {
        x: (a.x + b.x) / 2,
        y: (a.y + b.y) / 2 - 3,
        "text-anchor": "middle",
      });
      label.textContent = fit(e.rel, 18);
      g.append(label);
      svg.append(g);
    }

    for (const n of neighbors) {
      const p = pos.get(n.id);
      const g = svgEl("g", { class: "node" });
      g.append(svgEl("circle", { cx: p.x, cy: p.y, r: 9 }));
      const label = svgEl("text", {
        x: p.x,
        y: p.y + 24,
        "text-anchor": "middle",
      });
      label.textContent = fit(n.name || n.key || n.id, 22);
      g.append(label);
      g.addEventListener("click", () => {
        loadEntity(n.id).catch(() => {});
      });
      svg.append(g);
    }

    const center = svgEl("g", { class: "node center" });
    center.append(svgEl("circle", { cx: cx, cy: cy, r: 12 }));
    const label = svgEl("text", { x: cx, y: cy - 20, "text-anchor": "middle" });
    label.textContent = fit(entity.name, 26);
    center.append(label);
    svg.append(center);
  }

  // ---- documents + ingest -------------------------------------------------

  function statusBadge(status) {
    const span = el("span", "badge", status || "unknown");
    if (status) span.classList.add(status);
    return span;
  }

  async function loadDocuments() {
    const list = $("documents");
    list.replaceChildren(el("li", "muted", "Loading…"));
    let data;
    try {
      data = await api("/v1/documents");
    } catch (err) {
      fail(list, err);
      return;
    }
    const docs = (data && data.documents) || [];
    list.replaceChildren();
    if (!docs.length) {
      list.append(el("li", "muted", "Nothing ingested yet."));
      return;
    }
    for (const d of docs.slice(0, 50)) {
      const item = el("li");
      const head = el("div");
      head.append(statusBadge(d.status));
      head.append(document.createTextNode(d.title || d.source || d.id));
      item.append(head);
      const meta = el("div", "snippet", (d.source_kind || "?") + " · " + d.id);
      item.append(meta);
      item.addEventListener("click", () => {
        api("/v1/documents/" + encodeURIComponent(d.id) + "/status")
          .then((s) => {
            meta.textContent =
              (d.source_kind || "?") +
              " · " +
              s.status +
              (s.error ? " · " + s.error : "") +
              " · " +
              d.id;
            meta.className =
              s.status === "failed" ? "snippet error" : "snippet";
          })
          .catch((err) => {
            meta.textContent = String(err.message || err);
            meta.className = "snippet error";
          });
      });
      list.append(item);
    }
  }

  async function pollStatus(id, statusNode) {
    for (let i = 0; i < STATUS_POLL_MAX; i++) {
      let s;
      try {
        s = await api("/v1/documents/" + encodeURIComponent(id) + "/status");
      } catch (err) {
        statusNode.textContent =
          "status unavailable: " + String(err.message || err);
        return;
      }
      statusNode.textContent = "document " + id + " · " + s.status;
      if (
        s.status === "done" ||
        s.status === "failed" ||
        s.status === "superseded"
      ) {
        if (s.error) statusNode.textContent += " · " + s.error;
        await loadDocuments();
        return;
      }
      await new Promise((resolve) => setTimeout(resolve, STATUS_POLL_MS));
    }
    statusNode.textContent =
      "document " + id + " is still processing; refresh Documents later";
  }

  async function ingest() {
    const kind = $("ingest-kind").value;
    const body = $("ingest-body").value.trim();
    const source = $("ingest-source").value.trim();
    const statusNode = $("ingest-status");
    const submit = $("ingest-submit");
    if (!body) return;
    const payload = kind === "url" ? { url: body } : { text: body };
    if (source) payload.source = source;
    setBusy(submit, true, "Submitting…");
    statusNode.className = "muted";
    statusNode.textContent = "";
    try {
      const res = await api("/v1/ingest", { method: "POST", json: payload });
      statusNode.textContent =
        "document " +
        res.document_id +
        (res.noop ? " (unchanged, no-op)" : " queued");
      await pollStatus(res.document_id, statusNode);
    } catch (err) {
      statusNode.textContent = String(err.message || err);
      statusNode.className = "muted error";
    } finally {
      setBusy(submit, false);
    }
  }

  // ---- wiring -------------------------------------------------------------

  function wire() {
    $("search-form").addEventListener("submit", (ev) => {
      ev.preventDefault();
      const query = $("search-input").value.trim();
      if (query) search(query);
    });
    $("docs-refresh").addEventListener("click", () => {
      loadDocuments().catch(() => {});
    });
    $("ingest-form").addEventListener("submit", (ev) => {
      ev.preventDefault();
      ingest();
    });
    $("auth-save").addEventListener("click", () => {
      apiKey = $("auth-key").value.trim();
      try {
        localStorage.setItem(KEY_STORE, apiKey);
      } catch (_) {}
      $("auth-key").value = "";
      $("auth-bar").hidden = true;
      $("status").textContent = "";
      loadDocuments().catch(() => {});
    });
    $("status").textContent = apiKey ? "using a saved API key" : "";
    loadDocuments().catch(() => {});
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wire);
  } else {
    wire();
  }
})();
