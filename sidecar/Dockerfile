# gkg extraction sidecar: GLiNEREL runtime, weights baked in.
#
# The image is runtime-offline: the model is downloaded during the build
# and the container never phones home afterwards (acceptance #5).
FROM python:3.12-slim

# The checkpoint is selectable at BUILD time (314m default, 72m or any full
# repository id): the weights are baked into the image, so the runtime is
# strictly offline and the knob is a build arg, not a runtime override.
ARG GLINEREL_MODEL=314m
ENV PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    GLINEREL_MODEL=${GLINEREL_MODEL}

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