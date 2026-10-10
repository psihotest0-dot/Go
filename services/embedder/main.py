import os
import sys
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from typing import List
from sentence_transformers import SentenceTransformer

# Model configuration
MODEL_NAME = "paraphrase-multilingual-MiniLM-L12-v2"
MAX_BATCH_SIZE = 64

# Force CPU only
os.environ["CUDA_VISIBLE_DEVICES"] = "-1"

app = FastAPI(title="CPU Embedding Service")

# Global variable to hold the model
model = None

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
    texts: List[str]

@app.post("/embed")
def embed(request: EmbedRequest):
    if not request.texts:
        return {"model": MODEL_NAME, "dim": 384, "vectors": []}

    # Process in batches
    vectors = []
    for i in range(0, len(request.texts), MAX_BATCH_SIZE):
        batch_texts = request.texts[i:i + MAX_BATCH_SIZE]
        embeddings = model.encode(batch_texts, convert_to_numpy=True)
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
