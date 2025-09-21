# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.24-alpine AS builder
# Allow Go to auto-fetch the required toolchain if versions drift
ENV GOTOOLCHAIN=auto
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src

# Copy module files first for better caching
COPY search_crawler_service/go.mod search_crawler_service/go.sum ./
RUN go mod download

# Copy source
COPY search_crawler_service/ ./

# Build binary
RUN go build -o /out/crawler .

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

# Binary
COPY --from=builder /out/crawler /app/crawler

# Default config baked into image (can be overridden via volumes)
COPY deploy/ /app/deploy/

ENV CRAWLER_CONFIG_PATH=/app/deploy/crawler.config.yaml

EXPOSE 8082
ENTRYPOINT ["/app/crawler"]