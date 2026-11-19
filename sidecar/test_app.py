"""Contract tests for the gkg sidecar service.

The tests pin the frozen HTTP contract (design section 5.4) end to end with
a stub that accepts label mappings and returns entities with character offsets.
They exercise byte-offset conversion, relation endpoint resolution, strict
batching and honest health.
"""

import threading
from concurrent.futures import ThreadPoolExecutor

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
    assert "diagnostics" not in results[0]
    # Thresholds arrived as a label mapping.
    assert fake.entities_seen[0] == {"person": 0.4}
    assert fake.relations_seen[0] == {"wrote": 0.25}


SHARED_SPAN = {"text": "Città", "start": 0, "end": 5}
UNIQUE_SPAN = {"text": "Notes", "start": 12, "end": 17}


@pytest.mark.parametrize(
    ("head", "tail", "binding", "ambiguity"),
    [
        (SHARED_SPAN, UNIQUE_SPAN, None, ([0, 1], [2])),
        (UNIQUE_SPAN, SHARED_SPAN, None, ([2], [0, 1])),
        (1, UNIQUE_SPAN, (1, 2), None),
        ({**SHARED_SPAN, "index": 1}, UNIQUE_SPAN, (1, 2), None),
        ({**UNIQUE_SPAN, "index": 0}, {**SHARED_SPAN, "label": "work"}, (2, 1), None),
        ({**SHARED_SPAN, "index": 2}, UNIQUE_SPAN, None, ([0, 1], [2])),
        ({**SHARED_SPAN, "index": 0, "label": "work"}, UNIQUE_SPAN, (1, 2), None),
        ({**SHARED_SPAN, "index": 3}, UNIQUE_SPAN, None, ([0, 1], [2])),
        ({**SHARED_SPAN, "label": "work"}, UNIQUE_SPAN, (1, 2), None),
        (SHARED_SPAN, {"start": 6, "end": 11}, None, None),
        ({**SHARED_SPAN, "label": "person"}, UNIQUE_SPAN, None, None),
    ],
    ids=[
        "head",
        "tail",
        "index",
        "object-index",
        "conflict-unique-span",
        "conflict-shared-span",
        "conflict-label",
        "out-of-range-index",
        "label",
        "missing-span",
        "missing-label",
    ],
)
def test_extract_shared_span_relation(head, tail, binding, ambiguity):
    class SharedSpanModel:
        def extract(self, texts, entities, relations):
            return [
                {
                    "entities": [
                        {**SHARED_SPAN, "label": "place"},
                        {**SHARED_SPAN, "label": "work"},
                        {**UNIQUE_SPAN, "label": "work"},
                    ],
                    "relations": [
                        {"label": "mentions", "head": head, "tail": tail, "score": 0.7}
                    ],
                }
            ]

    resp = client_with(SharedSpanModel()).post(
        "/v1/extract",
        json={
            "chunks": [{"id": "c1", "text": "Città cites Notes"}],
            "entities": ["place", "work"],
            "relations": ["mentions"],
        },
    )
    assert resp.status_code == 200
    (result,) = resp.json()["results"]
    assert [e["label"] for e in result["entities"]] == ["place", "work", "work"]
    expected_relations = []
    if binding:
        expected_relations.append(
            {"label": "mentions", "head": binding[0], "tail": binding[1], "score": 0.7}
        )
    assert result["relations"] == expected_relations
    if ambiguity:
        assert result["diagnostics"] == [
            {
                "type": "ambiguous_relation",
                "label": "mentions",
                "score": 0.7,
                "head_candidates": ambiguity[0],
                "tail_candidates": ambiguity[1],
            }
        ]
    else:
        assert "diagnostics" not in result


@pytest.mark.parametrize("distinct_text", [False, True], ids=["identical", "distinct-text"])
def test_adapt_collapses_duplicate_span_candidates(distinct_text):
    first = {**SHARED_SPAN, "label": "place", "score": 0.6}
    entities = [first, {**first, "score": 0.9}, {**UNIQUE_SPAN, "label": "work"}]
    if distinct_text:
        entities.append({**first, "text": "CITTA"})
    result = sidecar._adapt(
        "Città cites Notes",
        {
            "entities": entities,
            "relations": [{"label": "mentions", "head": SHARED_SPAN, "tail": UNIQUE_SPAN}],
        },
    )
    assert [e["text"] for e in result["entities"]] == [e["text"] for e in entities]
    if distinct_text:
        assert result["relations"] == []
        assert result["diagnostics"] == [
            {
                "type": "ambiguous_relation",
                "label": "mentions",
                "score": 0.5,
                "head_candidates": [0, 3],
                "tail_candidates": [2],
            }
        ]
    else:
        assert result["relations"] == [{"label": "mentions", "head": 0, "tail": 2, "score": 0.5}]
        assert "diagnostics" not in result


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


def test_model_aliases_resolve():
    # The quickstart documents both shipped checkpoints by their short
    # names; both must resolve to real repositories.
    assert sidecar.resolve_model("314m") == "VAGOsolutions/SauerkrautLM-GLiNEREL-314m"
    assert sidecar.resolve_model("72m") == "VAGOsolutions/SauerkrautLM-GLiNEREL-72m"
    # Anything else is a full repository id, passed through unchanged.
    assert sidecar.resolve_model("org/model") == "org/model"


def test_concurrent_extracts_are_serialized_and_healthz_is_available(monkeypatch):
    entered = threading.Event()
    release = threading.Event()
    second_waiting = threading.Event()

    class ObservedLock:
        def __init__(self):
            self.lock = threading.Lock()
            self.attempts = 0
            self.guard = threading.Lock()

        def __enter__(self):
            with self.guard:
                self.attempts += 1
                if self.attempts == 2:
                    second_waiting.set()
            self.lock.acquire()

        def __exit__(self, *args):
            self.lock.release()

    class BlockingModel(FakeModel):
        def __init__(self):
            super().__init__()
            self.active = 0
            self.peak = 0
            self.guard = threading.Lock()

        def extract(self, *args):
            with self.guard:
                self.active += 1
                self.peak = max(self.peak, self.active)
            try:
                entered.set()
                assert release.wait(5), "test did not release inference"
                return super().extract(*args)
            finally:
                with self.guard:
                    self.active -= 1

    fake = BlockingModel()
    monkeypatch.setattr(sidecar, "_model", fake)
    monkeypatch.setattr(sidecar, "_inference_lock", ObservedLock())
    with TestClient(sidecar.app) as client, ThreadPoolExecutor(max_workers=3) as pool:
        def post():
            return client.post("/v1/extract", json={"chunks": [{"id": "c1", "text": "Ada"}]})

        first = pool.submit(post)
        try:
            assert entered.wait(5)
            second = pool.submit(post)
            assert second_waiting.wait(5), "second request did not reach the lock"
            assert not second.done()
            health = pool.submit(client.get, "/healthz").result(timeout=5)
            assert health.status_code == 200
            assert fake.peak == 1
        finally:
            release.set()
        assert first.result(timeout=5).status_code == 200
        assert second.result(timeout=5).status_code == 200
    assert fake.calls == [1, 1]
    assert fake.peak == 1


def test_inference_lock_released_after_error(monkeypatch):
    class FailingOnce(FakeModel):
        def extract(self, *args):
            if not self.calls:
                self.calls.append(0)
                raise RuntimeError("inference failed")
            return super().extract(*args)

    monkeypatch.setattr(sidecar, "_model", FailingOnce())
    client = TestClient(sidecar.app, raise_server_exceptions=False)
    payload = {"chunks": [{"id": "c1", "text": "Ada"}]}
    assert client.post("/v1/extract", json=payload).status_code == 500
    assert sidecar._inference_lock.acquire(timeout=1), "inference error leaked the lock"
    sidecar._inference_lock.release()
    assert client.post("/v1/extract", json=payload).status_code == 200


@pytest.mark.parametrize(
    "name,repository,revision",
    [
        ("314m", "VAGOsolutions/SauerkrautLM-GLiNEREL-314m", "9424e1eb432625b356ec73ea701c5d2b31cbc162"),
        ("72m", "VAGOsolutions/SauerkrautLM-GLiNEREL-72m", "34c024bb2ea85326526f6c5d150b9afe8bbe4808"),
    ],
)
def test_model_revision_pinned_at_download_and_load(monkeypatch, name, repository, revision):
    import sys
    from types import SimpleNamespace

    calls = []
    fake = SimpleNamespace(from_pretrained=lambda model, **kwargs: calls.append((model, kwargs)))
    monkeypatch.setitem(sys.modules, "glinerel_fast", SimpleNamespace(GLiNERel=fake))
    monkeypatch.delenv("GLINEREL_MODEL_REVISION", raising=False)
    sidecar.prefetch(name)
    sidecar.load_model(name)
    assert calls == [(repository, {"revision": revision})] * 2


def test_custom_model_requires_an_immutable_revision(monkeypatch):
    import sys
    from types import SimpleNamespace

    calls = []
    fake = SimpleNamespace(from_pretrained=lambda model, **kwargs: calls.append((model, kwargs)))
    monkeypatch.setitem(sys.modules, "glinerel_fast", SimpleNamespace(GLiNERel=fake))
    monkeypatch.delenv("GLINEREL_MODEL_REVISION", raising=False)
    with pytest.raises(ValueError, match="revision"):
        sidecar.load_model("org/custom")
    monkeypatch.setenv("GLINEREL_MODEL_REVISION", "main")
    with pytest.raises(ValueError, match="revision"):
        sidecar.prefetch("org/custom")
    assert calls == []
    revision = "a" * 40
    monkeypatch.setenv("GLINEREL_MODEL_REVISION", revision)
    sidecar.prefetch("org/custom")
    sidecar.load_model("org/custom")
    assert calls == [("org/custom", {"revision": revision})] * 2
