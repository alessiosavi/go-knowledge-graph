// gkg web UI: a read-only viewer over the existing REST endpoints
// (/v1/search, /v1/entities, /v1/traverse, /v1/documents, /v1/ingest),
// plus the explicit ingest box. No dependencies; every request is
// same-origin. Direct gkg bearer keys are saved in localStorage and can be
// forgotten. An edge Basic challenge clears a stale key and asks for a reload
// so the browser can sign in. Credentials never go in URLs.
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

  function forgetSavedKey() {
    apiKey = "";
    try {
      localStorage.removeItem(KEY_STORE);
    } catch (_) {}
    $("auth-key").value = "";
    $("auth-forget").hidden = true;
    $("status").textContent = "";
  }

  function unauthorized(res) {
    let message = "unauthorized — provide the API key";
    if (/^\s*Basic(?:\s|$)/i.test(res.headers.get("WWW-Authenticate") || "")) {
      forgetSavedKey();
      $("auth-bar").hidden = true;
      message = "Reload the page to sign in.";
      $("status").textContent = message;
    } else {
      showAuthBar();
    }
    return Object.assign(new Error(message), { classified: true });
  }

  // api talks to the gkg REST surface. A 401 selects Basic or bearer sign-in;
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
      throw unauthorized(res);
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

  const domainNames = new Map();
  const domainName = (id) => domainNames.get(id) || id;
  async function loadDomains() {
    try {
      const domains = await api("/v1/domains");
      domainNames.clear();
      for (const d of (Array.isArray(domains) ? domains : [])) {
        domainNames.set(d.id, d.name || d.id);
      }
      for (const id of ["search-domain", "ask-domain"]) {
        const picker = $(id);
        const selected = picker.value;
        const all = el("option", "", "All domains");
        all.value = "";
        picker.replaceChildren(all);
        for (const [key, name] of domainNames) {
          const option = el("option", "", name);
          option.value = key;
          picker.append(option);
        }
        picker.value = domainNames.has(selected) ? selected : "";
      }
    } catch (_) {
      // Search remains available when the picker cannot load.
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
        json: { query: query, kind: "auto", limit: 20, domain: $("search-domain").value },
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
      if (hit.domain) {
        head.append(el("span", "badge", domainName(hit.domain)));
      }
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

  let selectedEntityID = "";

  async function loadEntity(id) {
    selectedEntityID = id;
    const box = $("entity");
    box.replaceChildren(el("p", "muted", "Loading…"));
    const view = await api(
      "/v1/entities/" + encodeURIComponent(id) +
        ($("include-low-confidence").checked ? "?include_low_confidence=true" : ""),
    );
    box.replaceChildren();
    const title = el("p");
    title.append(el("strong", null, view.name || view.id));
    if (view.type) title.append(el("span", "badge", view.type));
    box.append(title);
    if (view.key && !view.key.startsWith("#") && !view.key.startsWith("@")) {
      box.append(el("p", "muted", view.key));
    }
    if (view.aliases && view.aliases.length) {
      box.append(el("p", "muted", "aliases: " + view.aliases.join(", ")));
    }
    box.append(el("p", "muted", (view.mention_count || 0) + " mentions"));
    if (view.hidden) {
      box.append(el("p", "muted", "Hidden: " + (view.hidden.unclear || 0) +
        " unclear, " + (view.hidden.unjudged || 0) + " unjudged, " +
        (view.hidden.rejected || 0) + " rejected facts"));
    }
    if (view.together && view.together.length) {
      const heading = el("p");
      heading.append(el("strong", null, "Appears with"));
      box.append(heading);
      for (const other of view.together) {
        const row = el("p");
        const link = el("a", "node-link", other.name || other.id);
        link.href = "#";
        link.addEventListener("click", (ev) => {
          ev.preventDefault();
          loadEntity(other.id).catch(() => {});
        });
        row.append(link, document.createTextNode(" "));
        if (other.type) row.append(el("span", "badge", other.type));
        const count = other.shared_chunks;
        row.append(el("span", "muted", count + (count === 1 ? " shared chunk" : " shared chunks")));
        box.append(row);
      }
    }
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
          ...($("include-low-confidence").checked ? { include_low_confidence: true } : {}),
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
      const caps = st && st.generation;
      $("ask-settings").hidden = !caps;
      if (caps) {
        $("ask-top-k").disabled = !caps.top_k;
        $("ask-top-k-note").hidden = !!caps.top_k;
        const reasoning = $("ask-reasoning");
        const previous = reasoning.value;
        const efforts = caps.reasoning_efforts || [];
        reasoning.replaceChildren(new Option("Provider default", ""));
        for (const effort of efforts) {
          reasoning.append(new Option(effort[0].toUpperCase() + effort.slice(1), effort));
        }
        reasoning.value = efforts.includes(previous) ? previous : "";
        reasoning.disabled = efforts.length === 0;
      }
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
    if (item.tool === "find_products" && Array.isArray(result.hits)) {
      for (const { product } of result.hits) {
        if (!product) continue;
        const line = el("div", "snippet");
        line.append(el("span", "badge", "product"));
        line.append(document.createTextNode([product.title, product.id].filter(Boolean).join(" · ")));
        nodes.push(line);
      }
      return nodes.length ? nodes : [jsonPreview(result)];
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

  // Only server-sanitized Markdown HTML reaches this insertion point.
  function insertMarkdown(body, html) {
    body.innerHTML = html;
    for (const link of body.querySelectorAll("a[href]")) {
      if (/^https?:/i.test(link.getAttribute("href") || "")) {
        link.target = "_blank";
        link.rel = "noopener noreferrer";
      }
    }
  }

  let answerURL = null;

  function setAnswerDownload(markdown) {
    const link = $("ask-download");
    if (answerURL) URL.revokeObjectURL(answerURL);
    answerURL = null;
    link.removeAttribute("href");
    link.hidden = !markdown;
    if (markdown) {
      const file = new Blob([markdown], { type: "text/markdown;charset=utf-8" });
      answerURL = URL.createObjectURL(file);
      link.href = answerURL;
    }
  }

  function renderAnswer(res) {
    setAnswerDownload(res.answer);
    const answer = $("ask-answer");
    answer.replaceChildren();
    if (res.answer_html) {
      const body = el("div", "markdown-body");
      insertMarkdown(body, res.answer_html);
      answer.append(body);
    } else if (res.answer) {
      answer.append(el("p", "answer", res.answer));
    }
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
  async function askViaStream(request, statusNode) {
    const headers = Object.assign(
      { "Content-Type": "application/json" },
      apiKey ? { Authorization: "Bearer " + apiKey } : {},
    );
    const res = await fetch("/v1/agent/ask/stream", {
      method: "POST",
      headers,
      body: JSON.stringify(request),
    });
    if (res.status === 401) {
      throw unauthorized(res);
    }
    if (!res.ok) {
      let detail = "HTTP " + res.status + " " + res.statusText;
      let classified = res.status === 400 || res.status === 422;
      try {
        const body = await res.json();
        if (body && body.detail) detail = body.detail;
        if (body && ["agent_error", "agent_timeout", "agent_disabled", "body_too_large", "auth_permanent", "auth_transient"].includes(body.error)) {
          classified = true;
        }
      } catch (_) {
        // keep the status line
      }
      // Validation and agent errors are terminal. An absent stream route
      // still allows the existing JSON fallback.
      throw Object.assign(new Error(detail), { classified });
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
        if (!ev || done) continue;
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
          statusNode.textContent = "";
          renderAnswer(done);
        } else if (ev.name === "error") {
          const detail =
            (ev.data && (ev.data.detail || ev.data.error)) ||
            "the agent failed";
          throw Object.assign(new Error(detail), { classified: true });
        }
      }
    }
    if (!done) throw new Error("the answer stream ended early");
  }

  function askPayload(question) {
    const request = { question, domain: $("ask-domain").value };
    if ($("ask-settings").hidden) return request;
    const generation = {};
    for (const [key, id] of [
      ["temperature", "ask-temperature"],
      ["top_p", "ask-top-p"],
      ["top_k", "ask-top-k"],
    ]) {
      const field = $(id);
      if (!field.disabled && field.value !== "") generation[key] = Number(field.value);
    }
    const effort = $("ask-reasoning");
    if (!effort.disabled && effort.value) generation.reasoning_effort = effort.value;
    if (Object.keys(generation).length) request.generation = generation;
    return request;
  }

  async function ask() {
    const question = $("ask-input").value.trim();
    const statusNode = $("ask-status");
    const submit = $("ask-submit");
    if (!question || submit.disabled) return;
    const request = askPayload(question);
    setAnswerDownload("");
    setBusy(submit, true, "Asking…");
    statusNode.className = "muted";
    statusNode.textContent = "thinking…";
    $("ask-answer").replaceChildren();
    $("ask-evidence").replaceChildren();
    try {
      if (askStreaming) {
        try {
          await askViaStream(request, statusNode);
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
        json: request,
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
      if (d.domain) {
        head.append(el("span", "badge", domainName(d.domain)));
      }
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
      let metadata = (d.source_kind || "?") + " · " + d.id;
      if (d.document_date) {
        metadata += " · " + d.document_date;
        if (d.document_date_source) metadata += " (" + d.document_date_source + ")";
      }
      item.append(el("div", "snippet", metadata));
      item.classList.add("hit");
      item.addEventListener("click", () => {
        openDocument(d.id).catch(() => {});
      });
      list.append(item);
    }
  }

  async function pollStatus(id, statusNode, message) {
    for (let i = 0; i < STATUS_POLL_MAX; i++) {
      let s;
      try {
        s = await api("/v1/documents/" + encodeURIComponent(id) + "/status");
      } catch (err) {
        statusNode.textContent =
          "status unavailable: " + String(err.message || err);
        return;
      }
      statusNode.textContent = message + " · " + s.status;
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
      message + " is still processing; refresh Documents later";
  }

  async function ingest() {
    const kind = $("ingest-kind").value;
    const body = $("ingest-body").value.trim();
    const source = $("ingest-source").value.trim();
    const labels = $("ingest-labels").value.trim();
    const date = $("ingest-date").value.trim();
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
      if (date) form.append("document_date", date);
      opts.form = form;
    } else {
      const payload = kind === "url" ? { url: body } : { text: body };
      if (source) payload.source = source;
      if (labels) payload.labels = labels;
      if (date) payload.document_date = date;
      opts.json = payload;
    }
    setBusy(submit, true, "Submitting…");
    statusNode.className = "muted";
    statusNode.textContent = "";
    try {
      const res = await api("/v1/ingest", opts);
      const message = "document " + res.document_id + (res.noop ? " (no reprocessing)" : "");
      statusNode.textContent = message + (res.noop ? "" : " queued");
      if (kind === "file") fileInput.value = "";
      await pollStatus(res.document_id, statusNode, message);
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
    if (doc.domain) {
      meta.append(el("span", "badge", domainName(doc.domain)));
    }
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
      insertMarkdown(body, html);
    } else {
      body.append(el("p", "muted", "No stored text."));
    }
  }

  function closeReader() {
    $("reader").hidden = true;
  }

  // ---- wiring -------------------------------------------------------------

  function wire() {
    $("include-low-confidence").addEventListener("change", () => {
      if (selectedEntityID) loadEntity(selectedEntityID).catch(() => {});
    });
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
    $("ask-settings").addEventListener("invalid", () => {
      $("ask-settings").open = true;
    }, true);
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
      $("auth-forget").hidden = !apiKey;
      $("status").textContent = "";
      loadDocuments().catch(() => {});
      probeAgent();
      loadDomains();
    });
    $("auth-forget").addEventListener("click", forgetSavedKey);
    $("auth-forget").hidden = !apiKey;
    $("status").textContent = apiKey ? "using a saved API key" : "";
    loadDocuments().catch(() => {});
    probeAgent();
    loadDomains();
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wire);
  } else {
    wire();
  }
})();
