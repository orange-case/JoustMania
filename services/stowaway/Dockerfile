# Multi-stage build for stowaway, the synthetic load of the Orange Case (UC-106, #2576)
#
# Same layout as the operator pad image (generic python:3.14-slim builder, Chainguard
# runtime, nonroot), without native extensions.

FROM python:3.14-slim AS builder

# uv >= 0.8 reads the lockfile (revision 3); keep near the uv CI runs (#2266)
RUN pip install --no-cache-dir --only-binary :all: uv==0.11.6

WORKDIR /app

COPY pyproject.toml uv.lock /app/

COPY proto/pyproject.toml /app/proto/
COPY proto/*.py /app/proto/
RUN python -m compileall -q proto/

COPY lib/pyproject.toml /app/lib/
COPY lib/*.py /app/lib/
RUN python -m compileall -q lib/

COPY services/stowaway/pyproject.toml /app/services/stowaway/

# #2266: third-party dependencies come from uv.lock (hashed, wheels only), never from a fresh resolve.
RUN uv export --frozen --no-dev --no-emit-workspace --package joustmania-stowaway -o /tmp/requirements.txt
RUN uv pip install --system --require-hashes --only-binary :all: -r /tmp/requirements.txt
# The workspace's own packages from this checkout (proto, lib, stowaway) must build; no third-party code runs.
RUN uv pip install --system --no-deps -e proto/ -e lib/ -e services/stowaway/ # NOSONAR

# Runtime stage - Chainguard python (glibc, Python 3.14), same digest as the other Python services.
FROM cgr.dev/chainguard/python:latest@sha256:678e879909418cd070927d0ba1ed018be98d43929db2457c37b9b9764703678c

# Dependency fingerprint baked at build time (#925); "dev" when built outside CI.
ARG DEPS_FINGERPRINT=dev
LABEL org.joustmania.deps-fingerprint="${DEPS_FINGERPRINT}"

WORKDIR /app

COPY --from=builder /usr/local/lib/python3.14/site-packages/ /usr/lib/python3.14/

COPY --from=builder /app/pyproject.toml /app/
COPY proto/ /app/proto/
COPY lib/ /app/lib/
COPY services/stowaway/ /app/services/stowaway/

ENV PYTHONPATH=/app
ENV PYTHONDONTWRITEBYTECODE=0

ENV OTEL_SERVICE_NAME=stowaway
ENV OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317

# No port and no HEALTHCHECK: stowaway serves nothing (a gRPC client of the game
# coordinator and the mock control API, services.md §2).
USER nonroot

# `docker compose run --rm stowaway run --challenge span-rounds` is the command line (UC-106 BR-007).
ENTRYPOINT ["python", "/app/services/stowaway/cli.py"]
CMD ["run"]
