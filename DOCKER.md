# Docker Instructions for Quirm

This guide explains how to use Quirm with Docker, either by pulling the pre-built image or building it yourself.

## Quick Start (Pre-built Image)

Quirm is available on the GitHub Container Registry. You can pull the latest stable version:

```bash
docker pull ghcr.io/codetease/quirm:latest
```

### Run with Docker

```bash
docker run -d \
  --name quirm \
  -p 8080:8080 \
  -e S3_ENDPOINT="https://s3.example.com" \
  -e S3_BUCKET="my-bucket" \
  -e S3_ACCESS_KEY="my-access-key" \
  -e S3_SECRET_KEY="my-secret-key" \
  -e S3_REGION="auto" \
  -v $(pwd)/cache_data:/app/cache_data \
  ghcr.io/codetease/quirm:latest
```

## Build from Source

If you prefer to build the image locally:

```bash
docker build -t quirm .
```

Then run it using the local tag:

```bash
docker run -d \
  --name quirm \
  -p 8080:8080 \
  -e S3_ENDPOINT="https://s3.example.com" \
  ... \
  quirm
```

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `S3_ENDPOINT` | The endpoint URL of your S3 compatible storage. | |
| `S3_BUCKET` | The name of the S3 bucket. | |
| `S3_ACCESS_KEY` | Your S3 Access Key. | |
| `S3_SECRET_KEY` | Your S3 Secret Key. | |
| `S3_REGION` | S3 Region. | `auto` |
| `PORT` | The port the application runs on. | `8080` |
| `CACHE_TTL_HOURS` | Cache Time-To-Live in hours. | `24` |
| `WATERMARK_PATH` | Path to the watermark image inside the container. | |
| `WATERMARK_OPACITY` | Opacity of the watermark (0.0 - 1.0). | `0.5` |
| `SECRET_KEY` | Secret key for HMAC signature validation. | |

## Advanced Usage

### Watermark Support

To use a watermark, mount the watermark file into the container and set `WATERMARK_PATH`.

```bash
docker run -d \
  --name quirm \
  -p 8080:8080 \
  -e S3_ENDPOINT="..." \
  ... \
  -e WATERMARK_PATH="/app/watermark.png" \
  -v $(pwd)/assets/watermark.png:/app/watermark.png \
  -v $(pwd)/cache_data:/app/cache_data \
  ghcr.io/codetease/quirm:latest
```

### Volumes

*   `/app/cache_data`: Map this volume to persist the image cache between restarts.
