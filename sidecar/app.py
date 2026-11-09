"""gkg extraction sidecar: GLiNEREL behind the frozen HTTP contract.

Contract (design section 5.4):

- ``GET /healthz`` -> ``{"status": "ok", "model": <name>}``; triggers the
  (cached) model load, so an unhealthy runtime answers 503 instead of the
  gate passing and failing later.
- ``POST /v1/extract`` with
  ``{"chunks": [{"id", "text"}], "entities": [str | {label, threshold}],
  "relations": [...]}`` returns
  ``{"results": [{"chunk_id", "entities": [{label, text, start, end,
  score}], "relations": [{label, head, tail, score}]}]}`` — one result per
  submitted chunk, in order, with **byte** offsets and relation
  ``head``/``tail`` as indexes into that chunk's entity list.

The runtime adapter (``_adapt``) converts the pinned ``glinerel-fast``
output (span objects, character offsets) into that wire shape. Weights are
baked into the image at build time, so the container runs fully offline.
"""

import os
import threading
from typing import Union

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

DEFAULT_ENTITY_THRESHOLD = 0.4
DEFAULT_RELATION_THRESHOLD = 0.25
BATCH_SIZE = int(os.environ.get("GLINEREL_BATCH", "8"))

# Short aliases for the shipped checkpoints; anything else is treated as a
# full Hugging Face repository id and passed through.
MODEL_ALIASES = {
    "314m": "VAGOsolutions/SauerkrautLM-GLiNEREL-314m",
}
MODEL_NAME = os.environ.get("GLINEREL_MODEL", "314m")

app = FastAPI(title="gkg-sidecar")

_model = None
_model_lock = threading.Lock()


def resolve_model(name: str) -> str:
    return MODEL_ALIASES.get(name, name)


def load_model(name: str):
    """Loads the pinned GLiNEREL runtime for ``name`` (weights cached)."""
    from glinerel_fast import GLiNERel

    return GLiNERel.from_pretrained(resolve_model(name))


def prefetch(name: str) -> None:
    """Downloads weights into the image's cache at build time."""
    load_model(name)


def get_model():
    global _model
    if _model is None:
        with _model_lock:
            if _model is None:
                _model = load_model(MODEL_NAME)
    return _model


class Chunk(BaseModel):
    id: str
    text: str


class LabelSpec(BaseModel):
    label: str
    threshold: float


class ExtractRequest(BaseModel):
    chunks: list[Chunk]
    entities: list[Union[str, LabelSpec]] = []
    relations: list[Union[str, LabelSpec]] = []


def _thresholds(specs, default_threshold):
    out = {}
    for spec in specs:
        if isinstance(spec, str):
            out[spec] = default_threshold
        else:
            out[spec.label] = spec.threshold
    return out


def _byte_offsets(text: str, start: int, end: int) -> tuple[int, int]:
    """Character offsets (code points) -> UTF-8 byte offsets."""
    return len(text[:start].encode("utf-8")), len(text[:end].encode("utf-8"))


def _adapt(text: str, raw: dict) -> dict:
    """Runtime result -> wire result for one chunk."""
    entities = []
    for e in raw.get("entities", []):
        start, end = _byte_offsets(text, e["start"], e["end"])
        entities.append(
            {
                "label": e["label"],
                "text": e["text"],
                "start": start,
                "end": end,
                "score": e.get("score", 0.5),
            }
        )

    def span_index(span):
        if isinstance(span, int):
            return span
        if isinstance(span, dict) and "start" in span and "end" in span:
            start, end = _byte_offsets(text, span["start"], span["end"])
            for i, e in enumerate(entities):
                if e["start"] == start and e["end"] == end:
                    return i
        return -1

    relations = []
    for r in raw.get("relations", []):
        head, tail = span_index(r.get("head")), span_index(r.get("tail"))
        if head < 0 or tail < 0:
            continue  # relation referencing an entity the adapter dropped
        relations.append(
            {
                "label": r["label"],
                "head": head,
                "tail": tail,
                "score": r.get("score", 0.5),
            }
        )
    return {"entities": entities, "relations": relations}


@app.get("/healthz")
def healthz():
    try:
        get_model()
    except Exception as exc:  # pragma: no cover - exercised via the 503 test
        raise HTTPException(status_code=503, detail=f"model load failed: {exc}")
    return {"status": "ok", "model": MODEL_NAME}


@app.post("/v1/extract")
def extract(req: ExtractRequest):
    if not req.chunks:
        return {"results": []}
    entities = _thresholds(req.entities, DEFAULT_ENTITY_THRESHOLD)
    relations = _thresholds(req.relations, DEFAULT_RELATION_THRESHOLD)
    model = get_model()
    results = []
    for start in range(0, len(req.chunks), BATCH_SIZE):
        batch = req.chunks[start : start + BATCH_SIZE]
        raw = model.extract([c.text for c in batch], entities, relations)
        if len(raw) != len(batch):
            raise HTTPException(
                status_code=500,
                detail=f"model returned {len(raw)} results for {len(batch)} chunks",
            )
        for chunk, res in zip(batch, raw, strict=True):
            results.append({"chunk_id": chunk.id, **_adapt(chunk.text, res)})
    return {"results": results}
