FROM golang:1.24 AS builder
ARG CGO_ENABLED=0
ARG APP_NAME
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN go build -o /app/$APP_NAME ./cmd/$APP_NAME

FROM ghcr.io/games-on-whales/base-app:edge AS output
ARG APP_NAME

WORKDIR /app

COPY images/$APP_NAME/startup.sh /app/entrypoint.sh
COPY --from=builder --chown=ubuntu:ubuntu --chmod=755 /app/$APP_NAME /app/$APP_NAME
ENTRYPOINT ["/app/entrypoint.sh"]