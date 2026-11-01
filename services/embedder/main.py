import os
import threading
from contextlib import asynccontextmanager
from typing import List

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field, field_validator
from sentence_transformers import SentenceTransformer

# Model configuration. MODEL_NAME and DIMENSION must match config.DefaultEmbedModel
# and config.VectorDimension in the Go backend, which refuses any other answer.
MODEL_NAME = "intfloat/multilingual-e5-small"
DIMENSION = 384
MAX_BATCH_SIZE = 64

# Force CPU only
os.environ["CUDA_VISIBLE_DEVICES"] = "-1"

# Global variable to hold the model
model = None
_model_lock = threading.Lock()


def embedding_dimension(loaded_model) -> int:
    """Size of the vectors of a loaded model.

    sentence-transformers renamed the method; the old name still works but warns
    (FutureWarning), so the new one is preferred when it exists.
    """
    getter = getattr(loaded_model, "get_embedding_dimension", None)
    if getter is None:
        getter = loaded_model.get_sentence_embedding_dimension
    return getter()


def load_model():
    global model
    print(f"Loading model {MODEL_NAME} on CPU...")
    # Initialize the model on CPU
    loaded = SentenceTransformer(MODEL_NAME, device="cpu")

    dim = embedding_dimension(loaded)
    print(f"Model loaded. Dimension: {dim}")

    # The database columns are vector(384): a different size can never be stored.
    if dim != DIMENSION:
        raise RuntimeError(f"Model dimension is {dim}, expected {DIMENSION}.")
    model = loaded


@asynccontextmanager
async def lifespan(_app: FastAPI):
    load_model()
    yield


app = FastAPI(title="CPU Embedding Service", lifespan=lifespan)


class EmbedRequest(BaseModel):
    texts: List[str] = Field(
        ...,
        max_length=256,
        description="List of texts to embed. Maximum 256 texts."
    )

    @field_validator('texts')
    def check_text_length(cls, v):
        for text in v:
            if len(text) > 10000:
                raise ValueError("Each text must be at most 10000 characters long")
        return v

@app.post("/embed")
def embed(request: EmbedRequest):
    if not request.texts:
        return {"model": MODEL_NAME, "dim": DIMENSION, "vectors": []}

    if model is None:
        raise HTTPException(status_code=503, detail="Model not loaded yet")

    # Process in batches
    vectors = []
    with _model_lock:
        for i in range(0, len(request.texts), MAX_BATCH_SIZE):
            batch_texts = request.texts[i:i + MAX_BATCH_SIZE]
            embeddings = model.encode(
                batch_texts,
                convert_to_numpy=True,
                normalize_embeddings=True
            )
            vectors.extend(embeddings.tolist())

    return {
        "model": MODEL_NAME,
        "dim": DIMENSION,
        "vectors": vectors
    }

@app.get("/health")
def health():
    if model is None:
        raise HTTPException(status_code=503, detail="Model not loaded yet")

    return {
        "status": "ok",
        "model": MODEL_NAME,
        "dim": embedding_dimension(model)
    }
