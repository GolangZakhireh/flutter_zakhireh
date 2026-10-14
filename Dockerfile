# Build stage
# We use golang:1.22-alpine for a small build footprint.
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Download dependencies first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build the binary
COPY . .
# CGO_ENABLED=0 ensures a static binary, crucial for running in alpine/scratch
RUN CGO_ENABLED=0 GOOS=linux go build -o flutterzakhireh ./cmd/server

# Run stage
# We use alpine instead of gcr.io/distroless because gcr.io is blocked
# in Iran (403 Forbidden). Alpine lives on docker.io, same registry as
# the golang builder image, so no extra registry dependency.
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -S -g 65532 appgroup && \
    adduser -S -u 65532 -G appgroup appuser

WORKDIR /

# Copy the binary from builder
COPY --from=builder /app/flutterzakhireh /flutterzakhireh
# Copy templates for the dashboard
COPY --from=builder /app/internal/dashboard/templates /internal/dashboard/templates

# Volume configuration
# /data/packages persists cached Flutter packages.
# /data/specs persists uploaded specs, workspaces, outputs, and logs.
# Pre-create both as root so the non-root user can write even without a mount.
# When bind-mounting host directories, set ownership on the host instead:
# chown -R 65532:65532 ./data/packages ./data/specs
RUN mkdir -p /data/packages /data/specs && chown -R 65532:65532 /data

USER 65532:65532

# Expose the default port
EXPOSE 8811

# Environment Variables
ENV FLUTTERZAKHIREH_PORT=:8811
ENV FLUTTERZAKHIREH_DATA_DIR=/data/packages
ENV FLUTTERZAKHIREH_SPECS_DIR=/data/specs
ENV FLUTTERZAKHIREH_UPSTREAM=https://pub.dev

ENTRYPOINT ["/flutterzakhireh"]
