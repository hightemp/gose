# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.23-alpine AS builder
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src

# Cache modules
COPY domain_search_service/go.mod ./
RUN go mod download

# Copy source
COPY domain_search_service/ ./

# Build
RUN go build -o /out/domain-search .

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

# Binary
COPY --from=builder /out/domain-search /app/domain-search

# Default configs (can be overridden with volumes)
COPY deploy/ /app/deploy/

# Config path; DB DSN comes from environment PG_DSN
ENV DOMAIN_SEARCH_CONFIG_PATH=/app/deploy/domain_search.config.yaml

ENTRYPOINT ["/app/domain-search"]