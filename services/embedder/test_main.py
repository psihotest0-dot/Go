import warnings

import pytest
from fastapi.testclient import TestClient

import main
from main import app, load_model

client = TestClient(app)


@pytest.fixture(scope="module", autouse=True)
def init_model():
    # Load the model once for the module; the tests that need it missing hide it.
    load_model()


def test_health_endpoint():
    response = client.get("/health")
    assert response.status_code == 200
    data = response.json()
    assert data["status"] == "ok"
    assert data["model"] == "intfloat/multilingual-e5-small"
    assert data["dim"] == 384

def test_embed_endpoint():
    texts = ["Това е тест.", "This is a test."]
    response = client.post("/embed", json={"texts": texts})
    assert response.status_code == 200
    data = response.json()

    assert data["model"] == "intfloat/multilingual-e5-small"
    assert data["dim"] == 384
    assert len(data["vectors"]) == 2
    assert len(data["vectors"][0]) == 384
    assert len(data["vectors"][1]) == 384


def test_lifespan_loads_the_model_at_startup(monkeypatch):
    monkeypatch.setattr(main, "model", None)

    # Entering the client runs the startup of the app.
    with TestClient(app) as started:
        assert main.model is not None
        assert started.get("/health").status_code == 200


def test_the_service_is_unavailable_until_the_model_is_loaded(monkeypatch):
    monkeypatch.setattr(main, "model", None)

    health = client.get("/health")
    embed = client.post("/embed", json={"texts": ["текст"]})

    assert health.status_code == 503
    assert embed.status_code == 503
    # Nothing to embed needs no model.
    assert client.post("/embed", json={"texts": []}).json()["vectors"] == []


def test_embed_rejects_too_many_and_too_long_texts():
    assert client.post("/embed", json={"texts": ["a"] * 257}).status_code == 422
    assert client.post("/embed", json={"texts": ["a" * 10001]}).status_code == 422


def test_embedding_dimension_uses_the_new_name_without_a_warning():
    # The old name only warns in current sentence-transformers; it must not be called.
    with warnings.catch_warnings():
        warnings.simplefilter("error")
        assert main.embedding_dimension(main.model) == 384


def test_embedding_dimension_falls_back_to_the_old_name():
    class OldModel:
        def get_sentence_embedding_dimension(self):
            return 384

    assert main.embedding_dimension(OldModel()) == 384


def test_a_model_of_the_wrong_size_is_refused(monkeypatch):
    class WrongModel:
        def get_embedding_dimension(self):
            return 768

    monkeypatch.setattr(main, "SentenceTransformer", lambda *args, **kwargs: WrongModel())
    monkeypatch.setattr(main, "model", None)

    with pytest.raises(RuntimeError, match="768"):
        load_model()
    assert main.model is None, "a refused model must not be served"
