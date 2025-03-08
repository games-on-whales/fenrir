FROM golang:1.24 AS builder
ARG CGO_ENABLED=0
ARG APP_NAME
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN go build -o /app/$APP_NAME ./cmd/$APP_NAME

FROM alpine:3.21.3 AS output
ARG APP_NAME

WORKDIR /app

COPY --from=builder --chmod=755 /app/$APP_NAME /app/$APP_NAME
ENTRYPOINT ["/app/$APP_NAME"]