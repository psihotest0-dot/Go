from fastapi.testclient import TestClient
from main import app, load_model
import pytest

client = TestClient(app)

@pytest.fixture(autouse=True)
def init_model():
    # Load model manually for tests
    load_model()

def test_health_endpoint():
    response = client.get("/health")
    assert response.status_code == 200
    data = response.json()
    assert data["status"] == "ok"
    assert data["model"] == "paraphrase-multilingual-MiniLM-L12-v2"
    assert data["dim"] == 384

def test_embed_endpoint():
    texts = ["Това е тест.", "This is a test."]
    response = client.post("/embed", json={"texts": texts})
    assert response.status_code == 200
    data = response.json()

    assert data["model"] == "paraphrase-multilingual-MiniLM-L12-v2"
    assert data["dim"] == 384
    assert len(data["vectors"]) == 2
    assert len(data["vectors"][0]) == 384
    assert len(data["vectors"][1]) == 384
