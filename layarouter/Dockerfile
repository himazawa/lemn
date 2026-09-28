FROM python:3.11-slim
RUN apt-get update && apt-get install -y --no-install-recommends curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -u 10001 -m laya
WORKDIR /app
COPY requirements.txt .
# Laya runs as a CPU classifier here; the default torch wheel would drag in the
# full CUDA stack (~9GB) that no container in this stack can use.
RUN pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cpu torch \
 && pip install --no-cache-dir -r requirements.txt
COPY server.py .
ENV USE_TF=0 HF_HOME=/hf-cache
RUN mkdir -p /hf-cache && chown laya:laya /hf-cache
USER laya
EXPOSE 8002
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "8002"]
