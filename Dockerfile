# Stage 1: Build
FROM golang:1.24-alpine AS builder

# Install build dependencies (needed for CGO)
# libvips-dev is required for govips
RUN apk add --no-cache gcc musl-dev vips-dev

WORKDIR /app

# Dependency caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build binary with CGO
# Note: govips relies on libvips shared libraries, so we cannot easily build a fully static binary
# unless we compile libvips statically too (which is complex). 
# We will build a dynamically linked binary and install libvips in the runtime image.
RUN CGO_ENABLED=1 GOOS=linux go build -o quirm ./main.go

# Stage 2: Runtime
FROM alpine:latest

# Install runtime dependencies for libvips
# poppler-glib is required for PDF support
RUN apk add --no-cache vips poppler-glib

WORKDIR /app

# Create non-root user for security
RUN adduser -D -g '' quirm

# Create directory for cache
RUN mkdir -p /app/cache_data && chown quirm:quirm /app/cache_data

# Copy binary from builder
COPY --from=builder /app/quirm .

# Switch to non-root user
USER quirm

# Expose port
EXPOSE 8080

# Entrypoint
ENTRYPOINT ["./quirm"]
