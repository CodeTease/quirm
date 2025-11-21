# Quirm

A lightweight, self-hosted asset delivery worker for S3-compatible storage services.

# Overview

Quirm acts as a performance layer between your S3 storage and the end-user. It fetches assets, applies compression (Brotli/Gzip) based on client capabilities, and serves them from a local disk cache to minimize egress costs and latency.

Supported backends: AWS S3, Cloudflare R2, MinIO, DigitalOcean Spaces, Wasabi, etc.

## Requirements

* Go 1.21+
* S3-compatible storage credentials

## Installation

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

Make a GET request to the server path corresponding to your object key:

`http://localhost:8080/images/logo.png`

This will fetch `images/logo.png` from your configured bucket.

## Configuration

Configuration is handled via environment variables in the `.env` file:

* `S3_ENDPOINT`: API Endpoint or Custom Domain URL of the storage provider.
* `S3_BUCKET`: The name of the bucket to serve assets from.
* `S3_REGION` (Default: `auto`): Bucket region (e.g., `us-east-1`).
* `S3_ACCESS_KEY` / `S3_SECRET_KEY`: API Credentials.
* `S3_FORCE_PATH_STYLE` (Default: `false`): Set true if your provider requires path-style addressing.
* `S3_USE_CUSTOM_DOMAIN` (Default: `false`): Crucial for Custom Domains/Public URLs. Set true to fix the double bucket name in the URL path.
* `CACHE_DIR` (Default: `./cache`): Local directory for storing cache files.
* `PORT` (Default: `8080`): Server listening port.
* `DEBUG` (Default: `false`): Set true to enable verbose AWS SDK logging for debugging requests.

## License

This project is under the **MIT License**.

> **ALPHA STAGE**