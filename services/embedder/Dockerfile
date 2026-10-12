FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .

RUN pip install --no-cache-dir -r requirements.txt

# Create an initial script to download the model at build time
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('paraphrase-multilingual-MiniLM-L12-v2', device='cpu')"

COPY . .

# Ensure CPU only
ENV CUDA_VISIBLE_DEVICES="-1"
# 1 worker as per requirement
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]
