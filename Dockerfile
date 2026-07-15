# Build stage
FROM golang:1.26-bookworm AS builder

WORKDIR /app

# Cache dependency downloads
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN mkdir -p /app/bin && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/bin/bot ./cmd/bot/

# Run stage
FROM debian:bookworm-slim

# Install runtime certificates and curl for the container health check
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN useradd -m -u 1000 botuser

# Create data directory for SQLite
RUN mkdir -p /data && chown botuser:botuser /data

COPY --from=builder /app/bin/bot /app/bot

USER botuser
WORKDIR /app

# Expose health-check / WebApp port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/health || exit 1

CMD ["/app/bot"]
