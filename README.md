# Quirm

A lightweight, self-hosted asset delivery worker for S3-compatible storage services with on-the-fly image processing.

**TL;DR:** Mini-Cloudinary 

# Overview

Quirm acts as a performance layer between your S3 storage and the end-user. It fetches assets, applies compression (Brotli/Gzip) based on client capabilities, and serves them from a local disk cache to minimize egress costs and latency.

It now supports **On-the-fly Image Processing**, allowing you to resize, crop, format convert, watermark, and generate blurhashes via URL parameters. It also includes advanced features like **Smart Crop**, **Face Detection**, and **Video Thumbnail Generation**.

Supported backends: AWS S3, Cloudflare R2, MinIO, DigitalOcean Spaces, Wasabi, etc.

## Requirements

* Go 1.24+
* S3-compatible storage credentials
* (Optional) Local watermark file
* (Optional) `ffmpeg` installed (for Video Thumbnail support)

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
* `focus`: Focus point for `fit=cover`. Options: `smart` (entropy), `face` (face detection).
* `q`: Quality (1-100). Default: 80.
* `format`: Output format (`jpeg`, `png`, `gif`, `webp`, `avif`).
* `text`: Text to overlay on the image.
* `color`: Text color (name or hex). Default: `red`.
* `ts`: Text size.
* `blurhash`: Set to `true` or `1` to return the Blurhash string of the image (content-type `text/plain`).
* `s`: URL Signature (Required if `SECRET_KEY` is set).

**Examples:**

* **Smart Crop (Auto-Focus):**
  `/images/banner.jpg?w=400&h=400&fit=cover&focus=smart`
* **Face Detection Crop:**
  `/images/avatar.jpg?w=200&h=200&fit=cover&focus=face`
* **Text Overlay:**
  `/images/sale.jpg?text=SALE+50%&color=white&ts=48`
* **Blurhash:**
  `/images/photo.jpg?blurhash=true`
* **Video Thumbnail:**
  `/videos/intro.mp4?w=300` (Requires `ENABLE_VIDEO_THUMBNAIL=true`)

### Auto-Format (AVIF/WebP)
If the client sends `Accept: image/avif` or `Accept: image/webp` header (most modern browsers), and no specific format is requested in the URL, Quirm automatically converts the image to the best available format (AVIF > WebP > Original) for optimal compression.

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
* `MAX_IMAGE_SIZE_MB`: Max input image size in MB (Default: 20).
* `ENABLE_METRICS`: Set to `true` to enable Prometheus metrics at `/metrics`. Default: `false`.
* `FACE_FINDER_PATH`: Path to the pigo cascade file for face detection. Default: `./facefinder`.

**Security & Advanced:**
* `ALLOWED_DOMAINS`: Comma-separated list of allowed domains for Referer/Origin checks.
* `RATE_LIMIT`: Requests per second limit per IP. Default: `10`.
* `ENABLE_VIDEO_THUMBNAIL`: Enable video thumbnail generation (Requires `ffmpeg`). Default: `false`.

**Cache:**
* `CACHE_DIR`: Directory for cache files.
* `CACHE_TTL_HOURS`: Cache expiration time in hours.
* `CLEANUP_INTERVAL_MINS`: How often to run garbage collection.

## Observability

Quirm supports Prometheus metrics for monitoring performance and health.

**Enable Metrics:**
Set `ENABLE_METRICS=true` in your environment.

**Endpoint:**
`GET /metrics`

**Available Metrics:**
* **HTTP:**
    * `quirm_http_requests_total`: Total requests by method, status, and path.
    * `quirm_http_request_duration_seconds`: Response latency histogram.
* **Cache:**
    * `quirm_cache_ops_total`: Cache Hits vs Misses (`type=hit|miss`). Use this to calculate Cache Hit Ratio.
* **Processing:**
    * `quirm_image_process_duration_seconds`: Time taken to resize/transform images.
    * `quirm_image_process_errors_total`: Count of processing failures.
* **Storage:**
    * `quirm_s3_fetch_duration_seconds`: Latency when fetching files from S3.

## License

This project is under the **MIT License**.

> **BETA STAGE**
