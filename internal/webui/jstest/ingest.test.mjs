import assert from "node:assert/strict";
import test from "node:test";
import { setImmediate } from "node:timers/promises";

import { page } from "./page.mjs";

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
  assert.equal(p.elements.get("ingest-status").textContent, "invalid document_date");
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
