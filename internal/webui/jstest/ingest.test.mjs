import assert from "node:assert/strict";
import test from "node:test";
import { setImmediate } from "node:timers/promises";

import { html, page } from "./page.mjs";

const domains = [
  { id: "d-catalogue", name: "Catalogue", documents: 2, products: 1, products_bound: true },
  { id: "d-manuals-a", name: "Manuals", documents: 1, products: 0, products_bound: false },
  { id: "d-manuals-b", name: "Manuals", documents: 0, products: 0, products_bound: false },
];

async function refreshDomains(p, reply) {
  p.replies.set("/v1/domains", reply);
  await p.fire("auth-save", "click");
}

function domainOptions(p, id = "ingest-domain") {
  const picker = p.elements.get(id);
  assert.ok(picker, `missing #${id}`);
  return picker.children.map((option) => [option.value, option.textContent]);
}

function ingestRequests(p) {
  return p.requests.filter(({ path, method }) => path === "/v1/ingest" && method === "POST");
}

test("ingest lists domains by name and disambiguates duplicate names by ID", async () => {
  const p = await page();
  await refreshDomains(p, domains);
  assert.ok(/<label for="ingest-domain">Domain<\/label>/.test(html), "missing ingest domain label");
  assert.deepEqual(domainOptions(p), [
    ["", "Auto (detect)"],
    ["d-catalogue", "Catalogue"],
    ["d-manuals-a", "Manuals (d-manuals-a)"],
    ["d-manuals-b", "Manuals (d-manuals-b)"],
  ]);
  assert.equal(p.elements.get("ingest-domain").value, "");
  assert.equal(p.elements.get("ingest-domain-note").textContent, "");
});

test("ingest disambiguates generated labels, literal labels, and the Auto label until unique", async () => {
  const p = await page();
  await refreshDomains(p, [
    ...domains,
    { id: "d-literal", name: "Manuals (d-manuals-a)" },
    { id: "d-twice", name: "Manuals (d-manuals-a) (d-manuals-a)" },
    { id: "d-auto", name: "Auto (detect)" },
    { id: "d-auto-literal", name: "Auto (detect) (d-auto)" },
  ]);

  assert.deepEqual(domainOptions(p), [
    ["", "Auto (detect)"],
    ["d-catalogue", "Catalogue"],
    ["d-manuals-a", "Manuals (d-manuals-a) (d-manuals-a) (d-manuals-a)"],
    ["d-manuals-b", "Manuals (d-manuals-b)"],
    ["d-literal", "Manuals (d-manuals-a) (d-literal)"],
    ["d-twice", "Manuals (d-manuals-a) (d-manuals-a) (d-twice)"],
    ["d-auto", "Auto (detect) (d-auto) (d-auto)"],
    ["d-auto-literal", "Auto (detect) (d-auto) (d-auto-literal)"],
  ]);
});

test("a failed domain refresh leaves only Auto for ingest and preserves search and ask filters", async () => {
  const p = await page();
  await refreshDomains(p, domains);
  const picker = p.elements.get("ingest-domain");
  assert.ok(picker, "missing ingest domain picker");
  picker.value = "d-catalogue";
  const filterIDs = ["search-domain", "ask-domain"];
  const filterOptions = filterIDs.map((id) => domainOptions(p, id));
  for (const id of filterIDs) p.elements.get(id).value = "d-catalogue";

  await refreshDomains(p, () => ({
    status: 503, ok: false,
    json: async () => ({ error: "store_error", message: "domains unavailable" }),
  }));

  assert.deepEqual(domainOptions(p), [["", "Auto (detect)"]]);
  assert.equal(picker.value, "");
  const note = p.elements.get("ingest-domain-note");
  assert.match(note.textContent, /domains unavailable/i);
  assert.match(note.textContent, /choose a domain again/i);
  assert.deepEqual(filterIDs.map((id) => domainOptions(p, id)), filterOptions);
  for (const id of filterIDs) assert.equal(p.elements.get(id).value, "d-catalogue");

  p.elements.get("ingest-kind").value = "text";
  p.elements.get("ingest-body").value = "catalogue page";
  await p.fire("ingest-form", "submit");
  assert.equal(ingestRequests(p).length, 0);

  picker.selectedIndex = 0;
  await p.fire("ingest-domain", "change");
  assert.equal(note.textContent, "domains unavailable");

  await refreshDomains(p, domains);
  assert.equal(picker.value, "");
  assert.equal(note.textContent, "");
});

test("a successful domain refresh keeps valid choices and blocks ingest when the selection vanishes", async () => {
  const p = await page();
  await refreshDomains(p, domains);
  const picker = p.elements.get("ingest-domain");
  assert.ok(picker, "missing ingest domain picker");
  p.elements.get("ingest-kind").value = "text";
  p.elements.get("ingest-body").value = "manual page";
  p.replies.set("/v1/ingest", { document_id: "submitted" });
  p.replies.set("/v1/documents/submitted/status", { status: "done" });
  picker.value = "d-manuals-b";
  await refreshDomains(p, domains);
  assert.equal(picker.value, "d-manuals-b");
  assert.equal(p.elements.get("ingest-domain-note").textContent, "");

  await refreshDomains(p, domains.slice(0, 2));
  assert.deepEqual(domainOptions(p), [
    ["", "Auto (detect)"], ["d-catalogue", "Catalogue"], ["d-manuals-a", "Manuals"],
  ]);
  assert.equal(picker.value, "");
  assert.match(p.elements.get("ingest-domain-note").textContent, /choose a domain again/i);
  await p.fire("ingest-form", "submit");
  assert.equal(ingestRequests(p).length, 0);
  assert.equal(p.elements.get("ingest-status").textContent, "Choose a domain again (or Auto) before submitting");

  picker.value = "d-manuals-a";
  await p.fire("ingest-domain", "change");
  assert.equal(p.elements.get("ingest-domain-note").textContent, "");
  assert.equal(p.elements.get("ingest-status").textContent, "");
  await p.fire("ingest-form", "submit");
  assert.deepEqual(ingestRequests(p).map(({ body }) => JSON.parse(body)), [
    { text: "manual page", domain: "d-manuals-a" },
  ]);
});

test("a second domain refresh cannot clear the warning before the user chooses Auto", async () => {
  const p = await page();
  await refreshDomains(p, domains);
  const picker = p.elements.get("ingest-domain");
  const note = p.elements.get("ingest-domain-note");
  picker.value = "d-catalogue";
  await refreshDomains(p, domains.slice(1));
  const warning = note.textContent;
  assert.match(warning, /choose a domain again/i);
  await refreshDomains(p, domains);
  assert.equal(note.textContent, warning);
  assert.equal(picker.selectedIndex, -1, "Auto must be available as a new selection");

  p.elements.get("ingest-kind").value = "text";
  p.elements.get("ingest-body").value = "catalogue page";
  p.replies.set("/v1/ingest", { document_id: "submitted" });
  p.replies.set("/v1/documents/submitted/status", { status: "done" });
  await p.fire("ingest-form", "submit");
  assert.equal(ingestRequests(p).length, 0);

  picker.selectedIndex = 0;
  await p.fire("ingest-domain", "change");
  assert.equal(note.textContent, "");
  await p.fire("ingest-form", "submit");
  assert.deepEqual(ingestRequests(p).map(({ body }) => JSON.parse(body)), [{ text: "catalogue page" }]);
});

test("an older failed refresh cannot reset the picker after a newer refresh succeeds", async () => {
  const p = await page();
  let finishOlderRefresh;
  await refreshDomains(p, () => new Promise((resolve) => { finishOlderRefresh = resolve; }));
  await refreshDomains(p, domains);
  const picker = p.elements.get("ingest-domain");
  assert.ok(picker, "missing ingest domain picker");
  picker.value = "d-catalogue";

  finishOlderRefresh({ status: 503, ok: false, json: async () => ({ error: "store_error" }) });
  await setImmediate();

  assert.equal(picker.value, "d-catalogue");
  assert.equal(p.elements.get("ingest-domain-note").textContent, "");
});

for (const kind of ["text", "url", "file"]) {
  test(`ingest sends a domain ID for ${kind} only when chosen`, async () => {
    const p = await page();
    await refreshDomains(p, domains);
    const get = (id) => p.elements.get(id);
    const picker = get("ingest-domain");
    assert.ok(picker, "missing ingest domain picker");
    get("ingest-kind").value = kind;
    get("ingest-body").value = kind === "url" ? "https://example.test/page" : "catalogue page";
    get("ingest-file").files = [new Blob(["catalogue page"], { type: "text/plain" })];
    p.replies.set("/v1/ingest", { document_id: "submitted" });
    p.replies.set("/v1/documents/submitted/status", { status: "done", domain: "d-manuals-b" });
    p.replies.set("/v1/documents", { documents: [
      { id: "submitted", status: "done", domain: "d-manuals-b" },
    ] });

    picker.value = "d-manuals-b";
    await p.fire("ingest-form", "submit");
    const chosen = p.requests.find((r) => r.path === "/v1/ingest");
    assert.ok(chosen, "missing ingest request");
    if (kind === "file") {
      assert.equal(chosen.body.get("domain"), "d-manuals-b");
    } else {
      assert.equal(JSON.parse(chosen.body).domain, "d-manuals-b");
    }
    assert.equal(get("ingest-status").textContent, "document submitted · done · Manuals");
    const documentHeading = get("documents").children[0].children[0];
    assert.ok(documentHeading.children.some((node) => node.textContent === "Manuals"));

    picker.value = "";
    await p.fire("ingest-form", "submit");
    const ingests = p.requests.filter((r) => r.path === "/v1/ingest");
    assert.equal(ingests.length, 2);
    const auto = ingests[1];
    if (kind === "file") {
      assert.equal(auto.body.has("domain"), false);
    } else {
      assert.equal(Object.hasOwn(JSON.parse(auto.body), "domain"), false);
    }
  });
}

test("an Auto ingest shows the detected domain name even when it was created during ingest", async () => {
  const p = await page();
  await refreshDomains(p, domains);
  p.replies.set("/v1/ingest", () => {
    p.replies.set("/v1/domains", [...domains,
      { id: "d-detected", name: "Detected domain", documents: 1, products: 0, products_bound: false },
    ]);
    return { status: 202, ok: true, json: async () => ({ document_id: "detected" }) };
  });
  p.replies.set("/v1/documents/detected/status", { id: "detected", status: "done", domain: "d-detected" });
  p.elements.get("ingest-kind").value = "text";
  p.elements.get("ingest-body").value = "catalogue page";

  await p.fire("ingest-form", "submit");

  assert.equal(p.elements.get("ingest-status").textContent, "document detected · done · Detected domain");
  assert.ok(!p.requests.some((r) => r.path === "/v1/documents/detected"), "completion must not fetch document details");
});

test("a completed ingest shows the status domain without fetching document details", async () => {
  const p = await page();
  await refreshDomains(p, domains);
  p.replies.set("/v1/ingest", { document_id: "completed" });
  p.replies.set("/v1/documents/completed/status", { id: "completed", status: "done", domain: "d-catalogue" });
  p.replies.set("/v1/documents/completed", () => ({
    status: 503, ok: false, json: async () => ({ error: "store_error", detail: "document unavailable" }),
  }));
  p.elements.get("ingest-kind").value = "text";
  p.elements.get("ingest-body").value = "catalogue page";

  await p.fire("ingest-form", "submit");

  assert.equal(p.elements.get("ingest-status").textContent, "document completed · done · Catalogue");
  assert.ok(!p.requests.some((r) => r.path === "/v1/documents/completed"), "completion must not fetch document details");
});

test("unknown_domain refreshes the pickers and keeps the draft while requiring a new choice", async () => {
  const p = await page();
  await refreshDomains(p, domains);
  const get = (id) => p.elements.get(id);
  const draft = [
    ["ingest-kind", "text"],
    ["ingest-body", "A fresh document"],
    ["ingest-source", "Draft manual"],
    ["ingest-labels", "documentation"],
    ["ingest-date", "2026-03-01"],
  ];
  for (const [id, value] of draft) get(id).value = value;
  get("ingest-domain").value = "d-catalogue";
  p.replies.set("/v1/domains", [
    domains[1], { id: "d-new", name: "New", documents: 0, products: 0, products_bound: false },
  ]);
  p.replies.set("/v1/ingest", () => ({
    status: 409, ok: false,
    json: async () => ({ error: "unknown_domain", detail: "domain d-catalogue disappeared" }),
  }));

  await p.fire("ingest-form", "submit");

  assert.equal(get("ingest-status").textContent, "unknown_domain: domain d-catalogue disappeared");
  for (const [id, value] of draft) assert.equal(get(id).value, value);
  assert.deepEqual(domainOptions(p), [["", "Auto (detect)"], ["d-manuals-a", "Manuals"], ["d-new", "New"]]);
  for (const id of ["search-domain", "ask-domain"]) {
    assert.deepEqual(domainOptions(p, id), [["", "All domains"], ["d-manuals-a", "Manuals"], ["d-new", "New"]]);
  }
  assert.match(get("ingest-domain-note").textContent, /choose a domain again/i);
  await p.fire("ingest-form", "submit");
  assert.equal(ingestRequests(p).length, 1);
  assert.equal(get("ingest-status").textContent, "Choose a domain again (or Auto) before submitting");
});

for (const [code, field, message, status] of [
  ["unknown_domain", "message", "domain d-removed does not exist; GET /v1/domains lists them", 400],
  ["ambiguous_domain", "message", "Manuals matches d-manuals-a and d-manuals-b", 400],
  ["domain_conflict", "detail", "product refs pin domain d-catalogue", 409],
  ["schema_not_v4", "detail", "explicit choices require extraction schema v4", 409],
]) {
  test(`ingest displays the REST ${code} code and message verbatim`, async () => {
    const p = await page();
    p.replies.set("/v1/ingest", () => ({
      status, ok: false,
      json: async () => ({ error: code, [field]: message }),
    }));
    p.elements.get("ingest-kind").value = "text";
    p.elements.get("ingest-body").value = "catalogue page";
    await p.fire("ingest-form", "submit");
    assert.equal(p.elements.get("ingest-status").textContent, `${code}: ${message}`);
  });
}

for (const kind of ["text", "url", "file"]) {
  test(`ingest sends document_date for ${kind}`, async () => {
    const p = await page();
    const get = (id) => p.elements.get(id);
    p.replies.set("/v1/ingest", { document_id: "dated", noop: true });
    p.replies.set("/v1/documents/dated/status", { status: "done" });
    p.replies.set("/v1/documents", { documents: [
      { id: "dated", status: "done", document_date: "2024-03-01T00:00:00Z", document_date_source: "caller" },
    ] });
    const dateInput = get("ingest-date");
    assert.ok(dateInput, "missing document date input");
    get("ingest-kind").value = kind;
    get("ingest-body").value = kind === "url" ? "https://example.test/page" : "catalogue page";
    get("ingest-file").files = [new Blob(["catalogue page"], { type: "text/plain" })];
    const date = "2024-03-01";
    dateInput.value = date;
    await p.fire("ingest-form", "submit");
    const request = p.requests.find((r) => r.path === "/v1/ingest");
    assert.ok(request, "missing ingest request");
    if (kind === "file") {
      assert.equal(request.body.get("document_date"), date);
      assert.equal(request.headers["Content-Type"], undefined);
    } else {
      assert.equal(JSON.parse(request.body).document_date, date);
    }
    assert.match(get("ingest-status").textContent, /no reprocessing/i);
    const metadata = get("documents").children[0].children[1].textContent;
    assert.match(metadata, /2024-03-01.*caller/);

    dateInput.value = "";
    await p.fire("ingest-form", "submit");
    const undated = p.requests.filter((r) => r.path === "/v1/ingest").at(-1);
    if (kind === "file") {
      assert.equal(undated.body.has("document_date"), false);
    } else {
      assert.equal(Object.hasOwn(JSON.parse(undated.body), "document_date"), false);
    }
  });
}

test("ingest displays an invalid document date from REST", async () => {
  const p = await page();
  p.replies.set("/v1/ingest", () => ({
    status: 400, ok: false,
    json: async () => ({ error: "invalid_request", detail: "invalid document_date" }),
  }));
  p.elements.get("ingest-kind").value = "text";
  p.elements.get("ingest-body").value = "catalogue page";
  const dateInput = p.elements.get("ingest-date");
  assert.ok(dateInput, "missing document date input");
  dateInput.value = "bad-date";
  await p.fire("ingest-form", "submit");
  assert.equal(p.elements.get("ingest-status").textContent, "invalid_request: invalid document_date");
});

test("the document reader shows the same date and origin as the list", async () => {
  const p = await page();
  const docs = [
    { id: "caller-dated", status: "done", document_date: "2024-03-01T00:00:00Z", document_date_source: "caller" },
    { id: "ingest-dated", status: "done", document_date: "2026-03-01T00:00:00Z", document_date_source: "ingest" },
    { id: "legacy", status: "done" },
  ];
  p.replies.set("/v1/documents", { documents: docs });
  await p.fire("docs-refresh", "click");
  const rows = p.elements.get("documents").children;
  for (const [i, doc] of docs.entries()) {
    p.replies.set(`/v1/documents/${doc.id}/content`, { document: doc, chunks: [] });
    rows[i].dispatch("click");
    await setImmediate();
    const metadata = p.elements.get("reader-meta").children.map((el) => el.textContent).join("");
    assert.equal(p.elements.get("reader-title").textContent, doc.id);
    const listMetadata = rows[i].children[1].textContent;
    if (doc.document_date) {
      const date = `${doc.document_date} (${doc.document_date_source})`;
      assert.ok(listMetadata.includes(date), listMetadata);
      assert.ok(metadata.includes(date), metadata);
    } else {
      assert.equal(metadata, "? · legacy · done · 0 chunks");
    }
  }
});
