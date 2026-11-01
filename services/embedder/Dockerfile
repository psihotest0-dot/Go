FROM python:3.11-slim

WORKDIR /app

# The model lives outside the home directory of root, so the service can read it
# as an unprivileged user.
ENV HF_HOME=/opt/hf-cache

COPY requirements.txt .

RUN pip install --no-cache-dir -r requirements.txt

# Create an initial script to download the model at build time
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('intfloat/multilingual-e5-small', device='cpu')"

# The service does not need root: it listens on a high port and only reads the
# model and the code.
RUN useradd --system --uid 10001 --no-create-home embedder
RUN chown -R embedder:embedder /opt/hf-cache

COPY . .

# Ensure CPU only
ENV CUDA_VISIBLE_DEVICES="-1"
# The model is in the image: never go to the internet at start (the hub check would
# slow the start down and fail without a connection).
ENV HF_HUB_OFFLINE=1

USER embedder

# The model takes a while to load, hence the long start period. /health answers
# 503 until it is loaded.
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 CMD python -c "import sys, urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/health', timeout=4).status == 200 else 1)"

# 1 worker as per requirement
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]
