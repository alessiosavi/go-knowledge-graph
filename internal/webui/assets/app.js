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
      body:
        opts.form instanceof FormData
          ? opts.form
          : opts.json
            ? JSON.stringify(opts.json)
            : undefined,
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
      } else if (hit.kind === "chunk" && hit.document_id) {
        item.classList.add("hit");
        item.addEventListener("click", () => {
          openDocument(hit.document_id).catch(() => {});
        });
        item.append(el("div", "snippet", "doc " + hit.document_id));
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
      if (f.doc_id) {
        const source = el("div");
        const link = el("a", "node-link", "doc " + f.doc_id);
        link.href = "#";
        link.addEventListener("click", (ev) => {
          ev.preventDefault();
          openDocument(f.doc_id).catch(() => {});
        });
        source.append(link);
        if (f.chunk_id) {
          source.append(document.createTextNode(" · " + f.chunk_id));
        }
        evi.append(source);
      }
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
    // Edges carry entity keys while nodes carry ids: place each node under
    // both so the edge lookup resolves under either convention.
    const pos = new Map();
    const place = (node, point) => {
      pos.set(node.key || node.id, point);
      pos.set(node.id, point);
    };
    place(entity, { x: cx, y: cy });
    neighbors.forEach((n, i) => {
      const angle = (2 * Math.PI * i) / neighbors.length - Math.PI / 2;
      place(n, {
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

  // ---- ask the graph -------------------------------------------------------

  // askStreaming mirrors GET /v1/agent/status's stream field: when true the
  // ask panel consumes the SSE endpoint; otherwise it uses the JSON one.
  let askStreaming = false;

  async function probeAgent() {
    try {
      const st = await api("/v1/agent/status");
      $("ask-panel").hidden = !(st && st.enabled);
      askStreaming = !!(st && st.enabled && st.stream);
    } catch (_) {
      $("ask-panel").hidden = true;
      askStreaming = false;
    }
  }

  function entityLink(id, label) {
    const link = el("a", "node-link", label);
    link.href = "#";
    link.addEventListener("click", (ev) => {
      ev.preventDefault();
      loadEntity(id).catch(() => {});
    });
    return link;
  }

  // jsonPreview caps an evidence payload the UI cannot summarize nicely.
  function jsonPreview(value) {
    let text;
    try {
      text = JSON.stringify(value);
    } catch (_) {
      text = String(value);
    }
    if (!text) text = "";
    if (text.length > 1200) text = text.slice(0, 1200) + "…";
    return el("pre", null, text);
  }

  // evidenceDetails renders one evidence entry: click-through for entities
  // and documents, capped summaries otherwise.
  function evidenceDetails(item) {
    const result =
      item.result && typeof item.result === "object" ? item.result : {};
    const nodes = [];
    if (typeof result.error === "string") {
      nodes.push(el("div", "snippet error", result.error));
      return nodes;
    }
    if (Array.isArray(result.hits)) {
      for (const h of result.hits.slice(0, 8)) {
        const line = el("div", "snippet");
        line.append(el("span", "badge", h.kind || "hit"));
        if (h.kind === "entity" && h.id) {
          line.append(entityLink(h.id, h.title || h.id));
        } else if (h.kind === "chunk" && h.document_id) {
          const link = el("a", "node-link", h.title || h.id || "chunk");
          link.href = "#";
          link.addEventListener("click", (ev) => {
            ev.preventDefault();
            openDocument(h.document_id).catch(() => {});
          });
          line.append(link);
        } else {
          line.append(document.createTextNode(h.title || h.id || ""));
        }
        if (h.snippet) line.append(el("div", null, h.snippet));
        nodes.push(line);
      }
      return nodes;
    }
    if (result.name && result.id) {
      const line = el("div", "snippet");
      line.append(el("span", "badge", result.type || "entity"));
      line.append(entityLink(result.id, result.name));
      line.append(
        document.createTextNode(
          " · " + (result.mention_count || 0) + " mentions",
        ),
      );
      nodes.push(line);
      return nodes;
    }
    if (Array.isArray(result.edges) && Array.isArray(result.nodes)) {
      for (const e of result.edges.slice(0, 8)) {
        const line = el("div", "snippet");
        line.append(entityLink(e.from, e.from));
        line.append(document.createTextNode(" —" + e.rel + "→ "));
        line.append(entityLink(e.to, e.to));
        nodes.push(line);
      }
      if (!result.edges.length) {
        nodes.push(el("div", "snippet", "no fact edges"));
      }
      return nodes;
    }
    if (Array.isArray(result.documents)) {
      for (const d of result.documents.slice(0, 8)) {
        const line = el("div", "snippet");
        line.append(el("span", "badge", d.status || "unknown"));
        if (d.id) {
          const link = el("a", "node-link", d.title || d.source || d.id);
          link.href = "#";
          link.addEventListener("click", (ev) => {
            ev.preventDefault();
            openDocument(d.id).catch(() => {});
          });
          line.append(link);
          line.append(document.createTextNode(" · " + d.id));
        } else {
          line.append(document.createTextNode(d.title || d.source || ""));
        }
        nodes.push(line);
      }
      return nodes;
    }
    if (Array.isArray(result.rows)) {
      nodes.push(el("div", "snippet", result.rows.length + " row(s)"));
      nodes.push(jsonPreview(result.rows.slice(0, 5)));
      return nodes;
    }
    nodes.push(jsonPreview(result));
    return nodes;
  }

  function renderAnswer(res) {
    const answer = $("ask-answer");
    answer.replaceChildren();
    if (res.answer) answer.append(el("p", "answer", res.answer));
    const list = $("ask-evidence");
    list.replaceChildren();
    const evidence = res.evidence || [];
    for (const item of evidence) {
      const li = el("li");
      const head = el("div");
      head.append(el("span", "badge", item.tool || "tool"));
      li.append(head);
      for (const node of evidenceDetails(item)) li.append(node);
      list.append(li);
    }
    if (evidence.length) {
      $("ask-status").textContent =
        evidence.length +
        (evidence.length === 1 ? " tool call" : " tool calls") +
        " · " +
        (res.steps || 0) +
        " step(s)";
    }
  }

  // parseSSEFrame parses one SSE block ("event: x\ndata: {...}"); comments
  // and frames without a JSON payload return null.
  function parseSSEFrame(frame) {
    let name = "message";
    let data = null;
    for (const line of frame.split("\n")) {
      if (line.startsWith("event:")) {
        name = line.slice(6).trim();
      } else if (line.startsWith("data:")) {
        try {
          data = JSON.parse(line.slice(5).trim());
        } catch (_) {
          data = null;
        }
      }
    }
    return data === null ? null : { name: name, data: data };
  }

  // toolEventRow renders a provisional evidence row for a running tool call;
  // the authoritative done payload replaces it.
  function toolEventRow(data) {
    const li = el("li");
    const head = el("div");
    head.append(el("span", "badge", (data && data.name) || "tool"));
    li.append(head);
    li.append(jsonPreview(data && data.arguments));
    return li;
  }

  // askViaStream runs one question against the SSE endpoint, rendering
  // delta fragments as they arrive and tool calls as provisional rows; the
  // done payload is authoritative and replaces everything. A classified
  // error event (the ask itself failed server-side) throws with
  // err.classified so ask() shows it; transport-level failures throw plain
  // errors so ask() can fall back to the JSON endpoint.
  async function askViaStream(question, statusNode) {
    const headers = Object.assign(
      { "Content-Type": "application/json" },
      apiKey ? { Authorization: "Bearer " + apiKey } : {},
    );
    const res = await fetch("/v1/agent/ask/stream", {
      method: "POST",
      headers,
      body: JSON.stringify({ question: question }),
    });
    if (res.status === 401) {
      showAuthBar();
      throw Object.assign(new Error("unauthorized — provide the API key"), {
        classified: true,
      });
    }
    if (!res.ok) {
      let detail = "HTTP " + res.status + " " + res.statusText;
      try {
        const body = await res.json();
        if (body && body.detail) detail = body.detail;
      } catch (_) {
        // keep the status line
      }
      throw new Error(detail);
    }
    if (!res.body || !res.body.getReader) {
      throw new Error("this browser cannot read streams");
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    const answer = $("ask-answer");
    const evidence = $("ask-evidence");
    let answerNode = null;
    let buffer = "";
    let done = null;
    for (;;) {
      let chunk;
      try {
        chunk = await reader.read();
      } catch (err) {
        // A read error after the authoritative payload is not worth a
        // second ask: keep it, the response is complete.
        if (done) break;
        throw err;
      }
      if (chunk.done) break;
      buffer += decoder.decode(chunk.value, { stream: true });
      let idx;
      while ((idx = buffer.indexOf("\n\n")) >= 0) {
        const frame = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 2);
        const ev = parseSSEFrame(frame);
        if (!ev) continue;
        if (ev.name === "delta") {
          if (!answerNode) {
            answerNode = el("p", "answer");
            answer.replaceChildren(answerNode);
          }
          answerNode.textContent += (ev.data && ev.data.text) || "";
        } else if (ev.name === "tool") {
          evidence.append(toolEventRow(ev.data));
        } else if (ev.name === "done") {
          done = ev.data;
        } else if (ev.name === "error") {
          const detail =
            (ev.data && (ev.data.detail || ev.data.error)) ||
            "the agent failed";
          throw Object.assign(new Error(detail), { classified: true });
        }
      }
    }
    if (!done) throw new Error("the answer stream ended early");
    statusNode.textContent = "";
    renderAnswer(done);
  }

  async function ask() {
    const question = $("ask-input").value.trim();
    const statusNode = $("ask-status");
    const submit = $("ask-submit");
    if (!question) return;
    setBusy(submit, true, "Asking…");
    statusNode.className = "muted";
    statusNode.textContent = "thinking…";
    $("ask-answer").replaceChildren();
    $("ask-evidence").replaceChildren();
    try {
      if (askStreaming) {
        try {
          await askViaStream(question, statusNode);
          return;
        } catch (err) {
          if (err && err.classified) {
            statusNode.textContent = String(err.message || err);
            statusNode.className = "muted error";
            return;
          }
          // Transport-level stream failure: fall back to the JSON endpoint.
        }
      }
      const res = await api("/v1/agent/ask", {
        method: "POST",
        json: { question: question },
      });
      statusNode.textContent = "";
      renderAnswer(res);
    } catch (err) {
      statusNode.textContent = String(err.message || err);
      statusNode.className = "muted error";
    } finally {
      setBusy(submit, false);
    }
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
      const del = el("button", "mini doc-del", "✕");
      del.type = "button";
      del.title = "delete this document";
      del.setAttribute("aria-label", "Delete " + (d.title || d.source || d.id));
      del.addEventListener("click", (ev) => {
        // The row itself opens the reader: keep the delete self-contained.
        ev.stopPropagation();
        deleteDocument(d.id).catch(() => {});
      });
      head.append(del);
      item.append(head);
      const meta = el("div", "snippet", (d.source_kind || "?") + " · " + d.id);
      item.append(meta);
      item.classList.add("hit");
      item.addEventListener("click", () => {
        openDocument(d.id).catch(() => {});
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
    const labels = $("ingest-labels").value.trim();
    const statusNode = $("ingest-status");
    const submit = $("ingest-submit");
    const fileInput = $("ingest-file");
    const file = kind === "file" && fileInput.files ? fileInput.files[0] : null;
    if (kind === "file" && !file) {
      statusNode.textContent = "choose a file first";
      statusNode.className = "muted error";
      return;
    }
    if (kind !== "file" && !body) return;
    const opts = { method: "POST" };
    if (kind === "file") {
      // FormData must go through untouched: the browser sets the multipart
      // Content-Type with the boundary itself.
      const form = new FormData();
      form.append("file", file);
      if (source) form.append("source", source);
      if (labels) form.append("labels", labels);
      opts.form = form;
    } else {
      const payload = kind === "url" ? { url: body } : { text: body };
      if (source) payload.source = source;
      if (labels) payload.labels = labels;
      opts.json = payload;
    }
    setBusy(submit, true, "Submitting…");
    statusNode.className = "muted";
    statusNode.textContent = "";
    try {
      const res = await api("/v1/ingest", opts);
      statusNode.textContent =
        "document " +
        res.document_id +
        (res.noop ? " (unchanged, no-op)" : " queued");
      if (kind === "file") fileInput.value = "";
      await pollStatus(res.document_id, statusNode);
    } catch (err) {
      statusNode.textContent = String(err.message || err);
      statusNode.className = "muted error";
    } finally {
      setBusy(submit, false);
    }
  }

  // deleteDocument removes a document with its whole version chain
  // (DELETE /v1/documents/{id}?chain=1): chunks, facts and mentions go too,
  // and there is no undo — hence the confirm. The API default stays
  // per-version; the UI sends the chain flag on purpose.
  async function deleteDocument(id) {
    if (
      !window.confirm(
        "Delete document " +
          id +
          "?\n\nChunks, facts and mentions are removed permanently — including older versions of the same source. This cannot be undone.",
      )
    ) {
      return;
    }
    const status = $("docs-status");
    status.className = "muted";
    status.textContent = "deleting…";
    try {
      const res = await api(
        "/v1/documents/" + encodeURIComponent(id) + "?chain=1",
        { method: "DELETE" },
      );
      const n = (res && res.deleted) || 0;
      status.textContent =
        "deleted " + n + (n === 1 ? " version" : " versions");
      await loadDocuments();
    } catch (err) {
      status.className = "muted error";
      status.textContent = String(err.message || err);
    }
  }

  // ---- document reader ----------------------------------------------------

  // openDocument shows the reader panel with the server-rendered markdown
  // (GET /v1/documents/{id}/content): the fold + render happens server-side,
  // the page injects the returned HTML and hardens external links.
  async function openDocument(id) {
    const panel = $("reader");
    panel.hidden = false;
    const title = $("reader-title");
    const meta = $("reader-meta");
    const banner = $("reader-banner");
    const body = $("reader-body");
    title.textContent = "Loading…";
    meta.replaceChildren();
    banner.hidden = true;
    banner.className = "";
    banner.textContent = "";
    body.replaceChildren();

    let data;
    try {
      data = await api("/v1/documents/" + encodeURIComponent(id) + "/content");
    } catch (err) {
      title.textContent = "Document " + id;
      banner.textContent = String(err.message || err);
      banner.className = "error";
      banner.hidden = false;
      return;
    }
    const doc = (data && data.document) || {};
    title.textContent = doc.title || doc.source || doc.id || id;
    meta.append(
      document.createTextNode(
        (doc.source_kind || "?") + " · " + (doc.id || id) + " · ",
      ),
    );
    meta.append(statusBadge(doc.status));
    const chunks = (data && data.chunks) || [];
    meta.append(
      document.createTextNode(
        " · " + chunks.length + (chunks.length === 1 ? " chunk" : " chunks"),
      ),
    );
    if (doc.status === "superseded") {
      banner.textContent =
        "Superseded version: the newer version is a separate entry.";
      banner.hidden = false;
    }
    if (doc.status === "failed" && doc.error) {
      banner.textContent = doc.error;
      banner.className = "error";
      banner.hidden = false;
    }
    const html = (data && data.html) || "";
    if (html) {
      body.innerHTML = html;
      for (const link of body.querySelectorAll("a[href]")) {
        if (/^https?:/i.test(link.getAttribute("href") || "")) {
          link.target = "_blank";
          link.rel = "noopener noreferrer";
        }
      }
    } else {
      body.append(el("p", "muted", "No stored text."));
    }
  }

  function closeReader() {
    $("reader").hidden = true;
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
    $("reader-close").addEventListener("click", () => {
      closeReader();
    });
    $("ingest-form").addEventListener("submit", (ev) => {
      ev.preventDefault();
      ingest();
    });
    // The file row and the textarea trade places with the kind; required
    // moves with visibility (a hidden required control blocks submit).
    $("ingest-kind").addEventListener("change", () => {
      const isFile = $("ingest-kind").value === "file";
      $("ingest-file-row").hidden = !isFile;
      $("ingest-body").hidden = isFile;
      $("ingest-body").required = !isFile;
      $("ingest-file").required = isFile;
    });
    // Picking a file prefills the source (editable: the same string is the
    // stable identity, so a renamed file still supersedes).
    $("ingest-file").addEventListener("change", () => {
      const f = $("ingest-file").files && $("ingest-file").files[0];
      if (f) $("ingest-source").value = f.name;
    });
    $("ask-form").addEventListener("submit", (ev) => {
      ev.preventDefault();
      ask();
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
      probeAgent();
    });
    $("status").textContent = apiKey ? "using a saved API key" : "";
    loadDocuments().catch(() => {});
    probeAgent();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wire);
  } else {
    wire();
  }
})();
