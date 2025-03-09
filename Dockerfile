FROM golang:1.24 AS builder
ARG CGO_ENABLED=0
ARG APP_NAME
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN go build -o /app/$APP_NAME ./cmd/$APP_NAME

# Second stage: minimal runtime
FROM alpine:3.21.3 AS output
WORKDIR /app

# Alpine sh uses busybox which doesnt expand $@
# We need to use a shell for entrypoint in order to expand the envar for app
# Bash properly expands $@ to forwards args, so use that.
RUN apk add --no-cache bash

# Set the APP_NAME explicitly as an ENV var so it's available in runtime
ARG APP_NAME
ENV APP_NAME=${APP_NAME}

COPY --from=builder --chmod=755 /app/${APP_NAME} /app/${APP_NAME}

ENTRYPOINT ["bash", "-c", "exec /app/${APP_NAME} \"$0\" \"$@\""]
CMD []
