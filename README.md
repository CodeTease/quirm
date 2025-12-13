# Quirm

A lightweight, self-hosted asset delivery worker for S3-compatible storage services with on-the-fly image processing.

**TL;DR:** Mini-Cloudinary 

# Overview

Quirm acts as a performance layer between your S3 storage and the end-user. It fetches assets, applies compression (Brotli/Gzip) based on client capabilities, and serves them from a local disk cache to minimize egress costs and latency.

It now supports **On-the-fly Image Processing**, allowing you to resize, crop, format convert, and watermark images via URL parameters.

Supported backends: AWS S3, Cloudflare R2, MinIO, DigitalOcean Spaces, Wasabi, etc.

## Requirements

* Go 1.24+
* S3-compatible storage credentials
* (Optional) Local watermark file

## Installation

### Docker (Recommended)

You can run Quirm quickly using the official Docker image:

```bash
docker run -d -p 8080:8080 -v $(pwd)/cache_data:/app/cache_data ghcr.io/codetease/quirm:latest
```

See [DOCKER.md](DOCKER.md) for full configuration details.

### Manual Installation

1. Clone the repository.
2. Copy `.env.example` to `.env` and configure your storage credentials.
3. Build the binary:
```bash
go build -o quirm
```

4. Run the application:
```bash
./quirm
```

## Usage

### Basic Retrieval
`http://localhost:8080/images/logo.png`

### Image Processing
Quirm supports image manipulation via query parameters.

**Parameters:**
* `w`: Width (px)
* `h`: Height (px)
* `fit`: Resize mode (`cover`, `contain`, `fill`). Default is basic resize.
* `q`: Quality (1-100). Default: 80.
* `format`: Output format (`jpeg`, `png`, `gif`, `webp`).
* `s`: URL Signature (Required if `SECRET_KEY` is set).

**Examples:**

* Resize to 800x600:
  `/images/banner.jpg?w=800&h=600`
* Create a thumbnail (Cover fit):
  `/images/banner.jpg?w=200&h=200&fit=cover`
* Convert to WebP with 90 quality:
  `/images/banner.jpg?format=webp&q=90`

### Auto-WebP Conversion
If the client sends `Accept: image/webp` header (most modern browsers), and no specific format is requested in the URL, Quirm automatically converts the image to WebP for better compression.

### Security: URL Signatures
To prevent resource exhaustion attacks (DDoS) via infinite resize combinations, you should set a `SECRET_KEY` in your `.env`.

When enabled, all requests with query parameters MUST include a valid signature `s`.

**Signature Generation (HMAC-SHA256):**
`s = HMAC_SHA256(SECRET_KEY, "PATH?sorted_params")`

Example:
Path: `/images/logo.png`
Params: `w=200`, `h=100`
String to sign: `/images/logo.png?h=100&w=200` (Note: keys are sorted alphabetically)

### Watermarking
Configure `WATERMARK_PATH` in `.env` to overlay a watermark image on all processed images. It is applied at the bottom-right corner.

## Configuration

Configuration is handled via environment variables in the `.env` file:

**Core:**
* `S3_ENDPOINT`: API Endpoint of the storage provider.
* `S3_BUCKET`: The name of the bucket.
* `S3_REGION`: Bucket region.
* `S3_ACCESS_KEY` / `S3_SECRET_KEY`: API Credentials.
* `PORT`: Server port (Default: `8080`).

**Image Processing:**
* `SECRET_KEY`: Secret string for validating URL signatures (Recommended for production).
* `WATERMARK_PATH`: Local path to a watermark image file (e.g., `./assets/logo_wm.png`).
* `WATERMARK_OPACITY`: Opacity of the watermark (0.0 - 1.0). Default: 0.5.

**Cache:**
* `CACHE_DIR`: Directory for cache files.
* `CACHE_TTL_HOURS`: Cache expiration time in hours.
* `CLEANUP_INTERVAL_MINS`: How often to run garbage collection.

## License

This project is under the **MIT License**.

> **ALPHA STAGE**
