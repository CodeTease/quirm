# Stage 1: Build
FROM golang:1.24-bookworm AS builder

# Install build dependencies for libvips and required libraries
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips-dev \
    libpoppler-glib-dev \
    build-essential \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=1 GOOS=linux go build -o quirm ./main.go

# Stage 2: Runtime
FROM debian:bookworm-slim

# Install runtime dependencies
# libvips and poppler-glib are required for image/PDF processing
# ca-certificates needed for HTTPS S3 calls
RUN apt-get update && apt-get install -y --no-install-recommends \
    libvips42 \
    libpoppler-glib8 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Create non-root user for security
RUN useradd -m quirm

# Copy required directories to prevent app crashes
COPY --from=builder /app/assets ./assets
COPY --from=builder /app/facefinder ./facefinder

# Default fallback image
ENV DEFAULT_IMAGE_PATH=/app/assets/Teaserverse_icon.png

# Copy binary from build stage
COPY --from=builder /app/quirm .

# Grant permissions to quirm user in app and cache directories
RUN mkdir -p /app/cache_data && chown -R quirm:quirm /app

USER quirm

# Environment variable for AI support (if place .so file in a specific folder)
# ENV ORT_LIB_PATH=/app/libs

EXPOSE 8080

ENTRYPOINT ["./quirm"]