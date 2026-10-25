import os
import sys
import threading
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field, field_validator
from typing import List
from sentence_transformers import SentenceTransformer

# Model configuration
MODEL_NAME = "intfloat/multilingual-e5-small"
MAX_BATCH_SIZE = 64

# Force CPU only
os.environ["CUDA_VISIBLE_DEVICES"] = "-1"

app = FastAPI(title="CPU Embedding Service")

# Global variable to hold the model
model = None
_model_lock = threading.Lock()

@app.on_event("startup")
def load_model():
    global model
    print(f"Loading model {MODEL_NAME} on CPU...")
    # Initialize the model on CPU
    model = SentenceTransformer(MODEL_NAME, device="cpu")

    dim = model.get_sentence_embedding_dimension()
    print(f"Model loaded. Dimension: {dim}")

    # Check if dimension is exactly 384
    if dim != 384:
        print(f"Error: Model dimension is {dim}, expected 384. Exiting.")
        sys.exit(1)

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
        return {"model": MODEL_NAME, "dim": 384, "vectors": []}

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
        "dim": 384,
        "vectors": vectors
    }

@app.get("/health")
def health():
    if model is None:
        raise HTTPException(status_code=503, detail="Model not loaded yet")

    return {
        "status": "ok",
        "model": MODEL_NAME,
        "dim": model.get_sentence_embedding_dimension()
    }
