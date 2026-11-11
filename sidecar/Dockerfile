# gkg extraction sidecar: GLiNEREL runtime, weights baked in.
#
# The image is runtime-offline: the model is downloaded during the build
# and the container never phones home afterwards (acceptance #5).
FROM python:3.15.2-slim@sha256:65a65fc7b30b06cb6df4930fd4650697cb23f4c2698ab81fc87e4b031bc36928

# The checkpoint is selectable at BUILD time (314m default, 72m or any full
# repository id): the weights are baked into the image, so the runtime is
# strictly offline and the knob is a build arg, not a runtime override.
ARG GLINEREL_MODEL=314m
# Custom repositories require an immutable, full commit SHA.
ARG GLINEREL_MODEL_REVISION=
ENV PIP_NO_CACHE_DIR=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    GLINEREL_MODEL=${GLINEREL_MODEL} \
    GLINEREL_MODEL_REVISION=${GLINEREL_MODEL_REVISION}

WORKDIR /app

# All dependencies, including CPU torch and test contracts, come from the
# resolver-generated, hashed lock. Never resolve fresh versions at install.
COPY requirements.lock .
RUN pip install --no-cache-dir --require-hashes \
    -r requirements.lock

COPY app.py .
# Bake the model weights into the image cache at build time.
RUN python -c "import app; app.prefetch(app.MODEL_NAME)"

# Runtime is strictly offline: huggingface_hub serves only the baked
# snapshot, so the container never phones home and upstream branch moves
# cannot silently change the served weights (acceptance #5).
ENV HF_HUB_OFFLINE=1

COPY . .

EXPOSE 8090
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8090"]
