"""Contract tests for the gkg sidecar service.

The tests pin the frozen HTTP contract (design section 5.4) end to end with
a stub that emits the *real* GLiNEREL runtime shapes (label mappings in,
span objects and character offsets out), so the adaptation layer is
exercised: byte-offset conversion, relation span -> entity-index mapping,
strict batching and honest health.
"""

import pytest
from fastapi.testclient import TestClient

import app as sidecar


class FakeModel:
    """Deterministic stand-in emitting the pinned runtime shapes."""

    def __init__(self, short_results=0):
        self.calls = []
        self.entities_seen = []
        self.relations_seen = []
        self.short_results = short_results

    def extract(self, texts, entities, relations):
        # entities/relations: mappings label -> threshold (runtime contract).
        self.calls.append(len(texts))
        self.entities_seen.append(dict(entities))
        self.relations_seen.append(dict(relations))
        if self.short_results:
            texts = texts[: len(texts) - self.short_results]
        results = []
        for text in texts:
            words = text.split()
            if not words:
                results.append({"entities": [], "relations": []})
                continue
            first, last = words[0], words[-1]
            fstart = text.find(first)
            lstart = text.rfind(last)
            e_label = next(iter(entities), "entity")
            entities_out = [
                {
                    "label": e_label,
                    "text": first,
                    "start": fstart,
                    "end": fstart + len(first),
                    "score": 0.9,
                }
            ]
            relations_out = []
            if len(words) > 1 and relations:
                r_label = next(iter(relations))
                entities_out.append(
                    {
                        "label": e_label,
                        "text": last,
                        "start": lstart,
                        "end": lstart + len(last),
                        "score": 0.8,
                    }
                )
                relations_out.append(
                    {
                        "label": r_label,
                        "head": {
                            "text": first,
                            "start": fstart,
                            "end": fstart + len(first),
                        },
                        "tail": {
                            "text": last,
                            "start": lstart,
                            "end": lstart + len(last),
                        },
                        "score": 0.7,
                    }
                )
            results.append({"entities": entities_out, "relations": relations_out})
        return results


def client_with(model):
    sidecar._model = model
    return TestClient(sidecar.app)


def test_healthz_reports_model():
    client = client_with(FakeModel())
    resp = client.get("/healthz")
    assert resp.status_code == 200
    body = resp.json()
    assert body["status"] == "ok"
    assert "model" in body


def test_healthz_503_when_model_cannot_load():
    sidecar._model = None

    def boom(name):
        raise RuntimeError("weights missing")

    original = sidecar.load_model
    sidecar.load_model = boom
    try:
        client = TestClient(sidecar.app)
        resp = client.get("/healthz")
        assert resp.status_code == 503
    finally:
        sidecar.load_model = original


def test_extract_contract_shape_and_thresholds():
    fake = FakeModel()
    client = client_with(fake)
    resp = client.post(
        "/v1/extract",
        json={
            "chunks": [{"id": "c1", "text": "Ada Lovelace wrote it."}],
            "entities": [{"label": "person", "threshold": 0.4}],
            "relations": [{"label": "wrote", "threshold": 0.25}],
        },
    )
    assert resp.status_code == 200
    results = resp.json()["results"]
    assert len(results) == 1
    assert results[0]["chunk_id"] == "c1"
    entity = results[0]["entities"][0]
    assert entity["label"] == "person"
    assert entity["text"] == "Ada"
    assert entity["start"] == 0 and entity["end"] == 3
    # Relation spans became mention indexes into the entity list.
    relation = results[0]["relations"][0]
    assert relation["label"] == "wrote"
    assert relation["head"] == 0 and relation["tail"] == 1
    # Thresholds arrived as a label mapping.
    assert fake.entities_seen[0] == {"person": 0.4}
    assert fake.relations_seen[0] == {"wrote": 0.25}


def test_extract_accepts_plain_string_labels():
    fake = FakeModel()
    client = client_with(fake)
    resp = client.post(
        "/v1/extract",
        json={
            "chunks": [{"id": "c1", "text": "Ada"}],
            "entities": ["person"],
            "relations": [],
        },
    )
    assert resp.status_code == 200
    assert fake.entities_seen[0] == {"person": sidecar.DEFAULT_ENTITY_THRESHOLD}


def test_extract_converts_char_offsets_to_bytes():
    fake = FakeModel()
    client = client_with(fake)
    # "Città" is 5 characters but 6 bytes: the runtime reports char offsets,
    # the wire contract requires byte offsets.
    resp = client.post(
        "/v1/extract",
        json={
            "chunks": [{"id": "c1", "text": "Città perché bella."}],
            "entities": ["place"],
            "relations": [],
        },
    )
    assert resp.status_code == 200
    entity = resp.json()["results"][0]["entities"][0]
    assert entity["text"] == "Città"
    assert entity["start"] == 0 and entity["end"] == 6, (
        "char offsets must be converted to bytes"
    )


def test_extract_batches_chunks():
    fake = FakeModel()
    client = client_with(fake)
    chunks = [{"id": f"c{i}", "text": f"word{i} text"} for i in range(9)]
    resp = client.post(
        "/v1/extract", json={"chunks": chunks, "entities": ["person"], "relations": []}
    )
    assert resp.status_code == 200
    assert len(resp.json()["results"]) == 9
    assert fake.calls == [8, 1]


def test_extract_short_model_response_is_500():
    fake = FakeModel(short_results=1)
    client = client_with(fake)
    chunks = [{"id": f"c{i}", "text": f"word{i} text"} for i in range(3)]
    resp = client.post(
        "/v1/extract", json={"chunks": chunks, "entities": ["person"], "relations": []}
    )
    assert resp.status_code == 500


def test_extract_empty_chunks():
    fake = FakeModel()
    client = client_with(fake)
    resp = client.post(
        "/v1/extract", json={"chunks": [], "entities": ["person"], "relations": []}
    )
    assert resp.status_code == 200
    assert resp.json() == {"results": []}
    assert fake.calls == []
