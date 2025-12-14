package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/CodeTease/quirm/pkg/cache"
	"github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/processor"
	"github.com/CodeTease/quirm/pkg/storage"
	"github.com/CodeTease/quirm/pkg/watermark"
)

type Handler struct {
	Config   config.Config
	S3       *storage.S3Client
	WM       *watermark.Manager
	Group    *singleflight.Group
	CacheDir string
}

func (h *Handler) HandleRequest(w http.ResponseWriter, r *http.Request) {
	cleanedPath := filepath.ToSlash(filepath.Clean(r.URL.Path))
	objectKey := strings.TrimPrefix(cleanedPath, "/")

	if strings.Contains(objectKey, "..") || objectKey == ".env" || objectKey == "" {
		http.Error(w, "Invalid Path", http.StatusBadRequest)
		return
	}

	queryParams := r.URL.Query()

	// 1. Security: Signature Verification
	if h.Config.SecretKey != "" && len(queryParams) > 0 {
		sig := queryParams.Get("s")
		if sig == "" {
			http.Error(w, "Missing signature", http.StatusForbidden)
			return
		}
		if !validateSignature(r.URL.Path, queryParams, h.Config.SecretKey) {
			http.Error(w, "Invalid signature", http.StatusForbidden)
			return
		}
	}

	// 2. Parse Image Options
	imgOpts := parseImageOptions(queryParams)

	// Determine Mode
	isImage := isImageFile(objectKey)

	// Auto-Format Logic: Check Accept Header
	if isImage && imgOpts.Format == "" {
		acceptHeader := r.Header.Get("Accept")
		if strings.Contains(acceptHeader, "image/avif") {
			imgOpts.Format = "avif"
		} else if strings.Contains(acceptHeader, "image/webp") {
			imgOpts.Format = "webp"
		}
	}

	shouldProcess := isImage && (imgOpts.Width > 0 || imgOpts.Height > 0 || imgOpts.Fit != "" || imgOpts.Format != "")

	cacheKey := ""
	encodingType := "identity"

	if shouldProcess {
		cacheKey = cache.GenerateKeyProcessed(objectKey, queryParams, imgOpts.Format)
	} else {
		// Passthrough Mode
		acceptEncoding := r.Header.Get("Accept-Encoding")
		if strings.Contains(acceptEncoding, "br") {
			encodingType = "br"
		} else if strings.Contains(acceptEncoding, "gzip") {
			encodingType = "gzip"
		}
		cacheKey = cache.GenerateKeyOriginal(objectKey, encodingType)
	}

	cacheFilePath := filepath.Join(h.CacheDir, cacheKey)

	_, err, _ := h.Group.Do(cacheKey, func() (interface{}, error) {
		if storage.FileExists(cacheFilePath) {
			return nil, nil
		}
		if h.Config.Debug {
			log.Printf("[MISS] Processing: %s (Key: %s)", objectKey, cacheKey)
		}

		if shouldProcess {
			return h.processAndSave(objectKey, cacheFilePath, imgOpts)
		} else {
			return h.fetchAndSave(objectKey, cacheFilePath, encodingType)
		}
	})

	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		log.Printf("Error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	serveFile(w, cacheFilePath, encodingType, objectKey, imgOpts.Format)
}

func (h *Handler) fetchAndSave(objectKey, destPath, encodingType string) (interface{}, error) {
	reader, _, err := h.S3.GetObject(context.TODO(), objectKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return nil, storage.AtomicWrite(destPath, reader, encodingType, h.CacheDir)
}

func (h *Handler) processAndSave(objectKey, destPath string, opts processor.ImageOptions) (interface{}, error) {
	reader, size, err := h.S3.GetObject(context.TODO(), objectKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	if h.Config.MaxImageSizeMB > 0 && size > h.Config.MaxImageSizeMB*1024*1024 {
		return nil, &FileSizeError{MaxSizeMB: h.Config.MaxImageSizeMB}
	}

	// Get watermark if configured
	wmImg, wmOpacity, err := h.WM.Get()
	if err != nil && h.Config.Debug {
		log.Printf("Error loading watermark: %v", err)
		// Continue without watermark? Or fail? The original code warned but continued.
	}

	buf, err := processor.Process(reader, opts, wmImg, wmOpacity, objectKey)
	if err != nil {
		return nil, err
	}

	return nil, storage.AtomicWrite(destPath, buf, "identity", h.CacheDir)
}

func validateSignature(path string, params url.Values, secret string) bool {
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "s" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(path)
	if len(keys) > 0 {
		b.WriteString("?")
	}
	for i, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(params.Get(k))
		if i < len(keys)-1 {
			b.WriteString("&")
		}
	}

	toSign := b.String()

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(toSign))
	expected := hex.EncodeToString(mac.Sum(nil))

	got := params.Get("s")
	return hmac.Equal([]byte(got), []byte(expected))
}

func parseImageOptions(params url.Values) processor.ImageOptions {
	opts := processor.ImageOptions{}
	if w := params.Get("w"); w != "" {
		opts.Width, _ = strconv.Atoi(w)
	}
	if h := params.Get("h"); h != "" {
		opts.Height, _ = strconv.Atoi(h)
	}
	opts.Fit = params.Get("fit")
	opts.Format = params.Get("format") // "jpeg", "png"
	if q := params.Get("q"); q != "" {
		opts.Quality, _ = strconv.Atoi(q)
	}
	return opts
}

func isImageFile(key string) bool {
	ext := strings.ToLower(filepath.Ext(key))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp"
}

func serveFile(w http.ResponseWriter, path string, encoding string, objectKey string, forcedFormat string) {
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "Cache miss mid-flight", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	now := time.Now()
	os.Chtimes(path, now, now)

	switch encoding {
	case "br":
		w.Header().Set("Content-Encoding", "br")
	case "gzip":
		w.Header().Set("Content-Encoding", "gzip")
	}

	mimeType := "application/octet-stream"

	// If processed, we trust forcedFormat. If not, we use objectKey extension.
	ext := ""
	if forcedFormat != "" {
		ext = "." + forcedFormat
	} else {
		ext = filepath.Ext(objectKey)
	}

	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		mimeType = "image/jpeg"
	case ".png":
		mimeType = "image/png"
	case ".gif":
		mimeType = "image/gif"
	case ".webp":
		mimeType = "image/webp"
	case ".avif":
		mimeType = "image/avif"
	case ".css":
		mimeType = "text/css"
	case ".js":
		mimeType = "application/javascript"
	case ".svg":
		mimeType = "image/svg+xml"
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, &http.Request{}, objectKey, time.Now(), file)
}
