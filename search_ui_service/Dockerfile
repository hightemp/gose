# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.23-alpine AS builder
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src

# Copy go module files for better caching
COPY search_ui_service/go.mod search_ui_service/go.sum ./
RUN go mod download

# Copy source
COPY search_ui_service/ ./

# Build binary
RUN go build -o /out/search-ui .

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

# Binary
COPY --from=builder /out/search-ui /app/search-ui

# Templates and default config (config can be overridden via volume)
COPY search_ui_service/templates/ /app/templates/
COPY deploy/ /app/deploy/

# Config path (DB DSN must be provided by environment PG_DSN)
ENV SEARCH_UI_CONFIG_PATH=/app/deploy/search_ui.config.yaml

EXPOSE 8080
ENTRYPOINT ["/app/search-ui"]