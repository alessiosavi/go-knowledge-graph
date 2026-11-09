# gkg extraction sidecar: GLiNEREL runtime, weights baked in.
#
# The image is runtime-offline: the model is downloaded during the build
# and the container never phones home afterwards (acceptance #5).
FROM python:3.12-slim

ENV PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    GLINEREL_MODEL=314m

WORKDIR /app

# CPU-only torch first, so the heavier GPU wheels never enter the image.
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .
# Bake the model weights into the image cache at build time.
RUN python -c "import app; app.prefetch(app.MODEL_NAME)"

# Runtime is strictly offline: huggingface_hub serves only the baked
# snapshot, so the container never phones home and upstream 'main' moves
# cannot silently change the served weights (acceptance #5).
ENV HF_HUB_OFFLINE=1

COPY . .

EXPOSE 8090
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8090"]