# Pinned by digest (Python 3.11.18): a rebuild months later starts from the same
# base the versions in requirements.txt were tested on. To move to a newer base,
# replace the digest (`docker buildx imagetools inspect python:3.11-slim`), run the
# tests in the new image and check that requirements.txt still installs.
FROM python:3.11-slim@sha256:673719e071ecf27434fd2635bbd8d00f49f4fdd81f243143438a42ced8bedf60

WORKDIR /app

# The model lives outside the home directory of root, so the service can read it
# as an unprivileged user.
ENV HF_HOME=/opt/hf-cache

COPY requirements.txt .

# Every version is pinned in requirements.txt, torch is the CPU-only build.
RUN pip install --no-cache-dir -r requirements.txt

# The service does not need root: it listens on a high port and only reads the
# model and the code. The cache directory is created for the service user before
# the download, so the 500 MB of the model are not copied again by a chown.
RUN useradd --system --uid 10001 --no-create-home embedder
RUN mkdir -p /opt/hf-cache && chown embedder:embedder /opt/hf-cache
USER embedder

# Download the model at build time
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('intfloat/multilingual-e5-small', device='cpu')"

COPY . .

# Ensure CPU only
ENV CUDA_VISIBLE_DEVICES="-1"
# The model is in the image: never go to the internet at start (the hub check would
# slow the start down and fail without a connection).
ENV HF_HUB_OFFLINE=1

# The model takes a while to load, hence the long start period. /health answers
# 503 until it is loaded.
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 CMD python -c "import sys, urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/health', timeout=4).status == 200 else 1)"

# 1 worker as per requirement.
#
# --timeout-keep-alive 120 is deliberately longer than the 90 s net/http keeps an
# idle connection (internal/llm/embedder.go): uvicorn's default of 5 s closed
# connections the backend still had in its pool, and a POST is not retried by
# net/http, so a request that landed in that window failed with "EOF" (seen on
# 2026-11-04 in a distillation job). The backend now closes idle connections
# first, which it does cleanly.
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1", "--timeout-keep-alive", "120"]
