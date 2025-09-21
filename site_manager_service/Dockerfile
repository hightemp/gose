# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.23-alpine AS builder
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src

# Copy go module files (site_manager_service has only go.mod)
COPY site_manager_service/go.mod ./
RUN go mod download

# Copy source
COPY site_manager_service/ ./

# Build binary
RUN go build -o /out/site-manager .

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app

# Binary
COPY --from=builder /out/site-manager /app/site-manager

# Templates and default config (can be overridden via volume)
COPY site_manager_service/templates/ /app/templates/
COPY deploy/ /app/deploy/

# Config path (DB DSN must be provided by environment PG_DSN)
ENV MANAGER_UI_CONFIG_PATH=/app/deploy/manager_ui.config.yaml

EXPOSE 8081
ENTRYPOINT ["/app/site-manager"]