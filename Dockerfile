# Stage 1: Build
FROM golang:1.24-alpine AS builder

# Install build dependencies (needed for CGO)
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Dependency caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static binary with CGO
# Note: CGO_ENABLED=1 is required for github.com/chai2010/webp, 
# but we link statically to create a self-contained binary.
RUN CGO_ENABLED=1 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o quirm ./main.go

# Stage 2: Runtime
FROM alpine:latest

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
