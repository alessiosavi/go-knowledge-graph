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
  Each result may include ``"diagnostics": [{type, label, score,
  head_candidates, tail_candidates}]``. Omission means empty. An
  ``ambiguous_relation`` diagnostic lists the possible entity indexes on
  each side of a relation withheld because an endpoint has multiple matches.

The runtime adapter (``_adapt``) converts the pinned ``glinerel-fast``
output (span objects, character offsets) into that wire shape. Weights are
baked into the image at build time, so the container runs fully offline.
"""

import os
import re
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
    "72m": "VAGOsolutions/SauerkrautLM-GLiNEREL-72m",
}
MODEL_REVISIONS = {
    MODEL_ALIASES["314m"]: "9424e1eb432625b356ec73ea701c5d2b31cbc162",
    MODEL_ALIASES["72m"]: "34c024bb2ea85326526f6c5d150b9afe8bbe4808",
}
MODEL_NAME = os.environ.get("GLINEREL_MODEL", "314m")

app = FastAPI(title="gkg-sidecar")

_model = None
_model_lock = threading.Lock()
_inference_lock = threading.Lock()


def resolve_model(name: str) -> str:
    return MODEL_ALIASES.get(name, name)


def load_model(name: str):
    """Loads the pinned GLiNEREL runtime for ``name`` (weights cached)."""
    from glinerel_fast import GLiNERel

    repository = resolve_model(name)
    revision = os.environ.get("GLINEREL_MODEL_REVISION") or MODEL_REVISIONS.get(repository, "")
    if not re.fullmatch(r"[0-9a-fA-F]{40}", revision):
        raise ValueError("model revision must be a full commit SHA; set GLINEREL_MODEL_REVISION")
    return GLiNERel.from_pretrained(repository, revision=revision)


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
    by_span = {}
    raw_entities = raw.get("entities", [])
    for i, e in enumerate(raw_entities):
        span = (e["start"], e["end"])
        # Keep the first relation candidate in each group of identical entities.
        by_span.setdefault(span, {}).setdefault((e["label"], e["text"]), i)
        start, end = _byte_offsets(text, *span)
        entities.append(
            {
                "label": e["label"],
                "text": e["text"],
                "start": start,
                "end": end,
                "score": e.get("score", 0.5),
            }
        )

    def endpoint_candidates(endpoint):
        if isinstance(endpoint, dict):
            index = endpoint.get("index")
            if "start" in endpoint and "end" in endpoint:
                span = (endpoint["start"], endpoint["end"])
                label = endpoint.get("label")
                if type(index) is int and 0 <= index < len(raw_entities):
                    entity = raw_entities[index]
                    if (entity["start"], entity["end"]) == span and (
                        label is None or entity["label"] == label
                    ):
                        return [index]
                candidates = by_span.get(span, {})
                return [
                    i for (name, _), i in candidates.items() if label is None or name == label
                ]
            endpoint = index
        if type(endpoint) is int and 0 <= endpoint < len(entities):
            return [endpoint]
        return []

    relations = []
    diagnostics = []
    for r in raw.get("relations", []):
        head = endpoint_candidates(r.get("head"))
        tail = endpoint_candidates(r.get("tail"))
        if not head or not tail:
            continue
        if len(head) > 1 or len(tail) > 1:
            diagnostics.append(
                {
                    "type": "ambiguous_relation",
                    "label": r["label"],
                    "score": r.get("score", 0.5),
                    "head_candidates": head,
                    "tail_candidates": tail,
                }
            )
            continue
        relations.append(
            {
                "label": r["label"],
                "head": head[0],
                "tail": tail[0],
                "score": r.get("score", 0.5),
            }
        )
    result = {"entities": entities, "relations": relations}
    if diagnostics:
        result["diagnostics"] = diagnostics
    return result


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
    # Each inference uses the available CPU threads. Concurrent sync handlers
    # queue here instead of oversubscribing them; healthz does not take this lock.
    with _inference_lock:
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
