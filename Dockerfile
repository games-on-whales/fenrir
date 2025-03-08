ARG APP_NAME

# Shared go builder
FROM golang:1.24 AS builder
ARG CGO_ENABLED=0
ARG APP_NAME
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY SOURCE_DIRECTORY .

RUN go build ./cmd/$APP_NAME -O /app/$APP_NAME

# wolf-agent build
FROM ghcr.io/games-on-whales/base-app:edge AS wolf-agent

RUN apt-get update && apt-get install -y python3-venv curl
RUN python3 -m venv /app/venv
RUN /app/venv/bin/pip install --no-cache-dir --upgrade pip
RUN /app/venv/bin/pip install --no-cache-dir --upgrade requests-unixsocket2

COPY --chown=ubuntu:ubuntu --chmod=755 images/wolf-agent/startup.sh /opt/gow/startup.sh
COPY --chown=ubuntu:ubuntu --chmod=755 images/wolf-agent/script.py /app/wolf-agent.py
COPY --from=builder --chown=ubuntu:ubuntu /app/wolf-agent /app/wolf-agent

ENV PATH="/app/venv/bin:${PATH}"
ENTRYPOINT ["/opt/gow/startup.sh"]

FROM ${APP_NAME} AS output