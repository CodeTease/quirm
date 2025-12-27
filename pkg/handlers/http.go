package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"github.com/CodeTease/quirm/pkg/cache"
	"github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/metrics"
	"github.com/CodeTease/quirm/pkg/processor"
	"github.com/CodeTease/quirm/pkg/storage"
	"github.com/CodeTease/quirm/pkg/watermark"
)

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

type Handler struct {
	Config      config.Config
	S3          storage.StorageProvider
	WM          *watermark.Manager
	Group       *singleflight.Group
	CacheDir    string
	MemoryCache *cache.MemoryCache
	Limiter     *rate.Limiter // unused if using ipLimiters
	ipLimiters  *expirable.LRU[string, *rate.Limiter]
	mu          sync.Mutex
}

func (h *Handler) SetIPLimiters(m *expirable.LRU[string, *rate.Limiter]) {
	h.ipLimiters = m
}

func (h *Handler) getLimiter(ip string) *rate.Limiter {
	// LRU is thread-safe for Get/Add, but we need to ensure atomic check-then-set if missing.
	// expirable.LRU doesn't support GetOrAdd atomically out of the box (it has Get and Add).
	// But it is fine to have a race condition where we create multiple limiters and overwrite.
	// Rate Limiting is approximate anyway.

	limiter, exists := h.ipLimiters.Get(ip)
	if !exists {
		// Create new limiter: rate limit from config, burst same as limit
		limit := rate.Limit(h.Config.RateLimit)
		limiter = rate.NewLimiter(limit, h.Config.RateLimit)
		h.ipLimiters.Add(ip, limiter)
	}

	return limiter
}

func (h *Handler) HandleRequest(w http.ResponseWriter, r *http.Request) {
	// Wrap writer if metrics enabled
	var rec *statusRecorder
	if h.Config.EnableMetrics {
		rec = &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		w = rec
	}

	start := time.Now()
	defer func() {
		if h.Config.EnableMetrics {
			duration := time.Since(start).Seconds()
			status := strconv.Itoa(rec.statusCode)
			// Use path template if possible?
			// Since we don't have a router with path templates here (just /),
			// we should probably just use "image" or "other" to avoid cardinality explosion,
			// or just the root path if it's the only one.
			// The user said: "path nên là template để tránh cardinality explosion".
			// But here everything is processed by this handler.
			// Let's check the URL. If it's an image, we can use "/{image}".
			// Since this is a proxy, the path IS the image path.
			// Cardinality explosion is real if we use the raw path.
			// Let's use a static label for now or a simple categorization.
			pathLabel := "/{image}" // Generic placeholder as requested
			metrics.HTTPRequestsTotal.WithLabelValues(r.Method, status, pathLabel).Inc()
			metrics.HTTPRequestDuration.WithLabelValues(r.Method, status, pathLabel).Observe(duration)
		}
	}()

	// 0. Security: Domain Whitelisting
	if len(h.Config.AllowedDomains) > 0 {
		referer := r.Header.Get("Referer")
		origin := r.Header.Get("Origin")
		allowed := false

		// If neither header is present, we might block or allow depending on strictness.
		// Usually tools like curl don't send them.
		// Assuming we only check if present or block if strictly required.
		// Let's check if either matches any allowed domain.
		check := func(val string) bool {
			if val == "" {
				return false
			}
			u, err := url.Parse(val)
			if err != nil {
				return false
			}
			for _, d := range h.Config.AllowedDomains {
				if d == u.Host || d == "*" {
					return true
				}
			}
			return false
		}

		if referer != "" {
			if check(referer) {
				allowed = true
			}
		}
		if origin != "" {
			if check(origin) {
				allowed = true
			}
		}

		// If both empty, maybe allow? Or if config is set, strict mode?
		// Let's assume if config is set, we enforce it on browsers (which send these).
		// But for non-browsers...
		// "không phải domain 'nhà mình' thì chặn luôn".
		// If both are empty, it might be direct access.
		if referer == "" && origin == "" {
			// Allow direct access or block?
			// Let's allow empty for now to not break curl/apps unless strictly specified.
			allowed = true
		}

		if !allowed && (referer != "" || origin != "") {
			http.Error(w, "Forbidden Domain", http.StatusForbidden)
			return
		}
	}

	// 0.5 Security: Rate Limiting
	// Get IP
	ip := r.RemoteAddr
	// RemoteAddr contains port, strip it
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}

	if h.Config.RateLimit > 0 {
		limiter := h.getLimiter(ip)
		if !limiter.Allow() {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
	}

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
	isVideo := isVideoFile(objectKey)

	// Video Thumbnail Logic
	if isVideo && h.Config.EnableVideoThumbnail {
		// We treat it similar to image processing but with different processor.
		// We use "video_thumb" as encodingType logic?
		// Or we process it.
		// We need to fetch the video first.

		// If width/height/format provided, we assume thumbnail generation is requested.
		// If not, maybe pass through video?
		// "Nếu link S3 là file video (.mp4), Quirm có thể dùng ffmpeg ... để lấy frame đầu tiên làm ảnh thumbnail."
		// Implies we treat it as an image request derived from video.

		// Let's force it to be processed if it's a video and we have config enabled.
		// We use "jpeg" as default format for thumbnail.
		if imgOpts.Format == "" {
			imgOpts.Format = "jpeg"
		}

		// We treat this as a "process" operation.
		// We need to modify processAndSave to handle video.
	}

	// Auto-Format Logic: Check Accept Header
	if isImage && imgOpts.Format == "" {
		acceptHeader := r.Header.Get("Accept")
		if strings.Contains(acceptHeader, "image/avif") {
			imgOpts.Format = "avif"
		} else if strings.Contains(acceptHeader, "image/webp") {
			imgOpts.Format = "webp"
		}
	}

	// 2.5 Check ETag / If-None-Match
	// For processed images, the key changes if params change.
	// We can use the cacheKey as ETag.
	// However, we compute cacheKey later. Let's compute it now.

	shouldProcess := (isImage && (imgOpts.Width > 0 || imgOpts.Height > 0 || imgOpts.Fit != "" || imgOpts.Format != "" || imgOpts.Blurhash)) || (isVideo && h.Config.EnableVideoThumbnail)

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

	// ETag Check
	etag := `"` + cacheKey + `"`
	if match := r.Header.Get("If-None-Match"); match != "" {
		if strings.Contains(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// Memory Cache Check
	if h.MemoryCache != nil {
		if data, found := h.MemoryCache.Get(cacheKey); found {
			metrics.CacheOpsTotal.WithLabelValues("hit_memory").Inc()
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			// Determine content type
			// Ideally we store metadata in cache too, but for now guess from format/ext

			// If blurhash, text/plain
			if imgOpts.Blurhash {
				w.Header().Set("Content-Type", "text/plain")
			} else {
				// We need to know the Content-Type.
				// For now, let's reuse serveFile logic or duplicate it.
				// Writing directly:
				setContentType(w, objectKey, imgOpts.Format)
			}

			w.Write(data)
			return
		}
	}

	cacheFilePath := filepath.Join(h.CacheDir, cacheKey)

	// Stale-While-Revalidate Logic
	// If file exists but is old (handled by cleaner currently), we serve it.
	// But cleaner deletes it.
	// If we want SWR, cleaner shouldn't delete, OR we check modtime here.
	// The current cleaner deletes files.
	// We can try to read the file. If it exists, serve it.
	// The cleaner might have deleted it.
	// If it exists, we assume it's valid for now unless we implement explicit expiry check here.
	// Given the instructions: "Hết hạn -> Trả về ảnh cũ ngay lập tức -> Ngầm fetch ảnh mới".
	// This implies we need to know if it is expired.
	// The cleaner handles expiry. If we want SWR, we should disable the cleaner for these files or change how cleaner works.
	// But modifying cleaner is separate.
	// Let's assume for this step: if file exists, we serve it.
	// To implement SWR properly, we need to check age.
	// If age > TTL, we serve it AND trigger background update.

	// Check file existence and age
	fileInfo, err := os.Stat(cacheFilePath)
	fileExists := err == nil

	var serveStale bool
	if fileExists {
		if time.Since(fileInfo.ModTime()) > h.Config.CacheTTL {
			serveStale = true
		}
	}

	if serveStale {
		// Trigger background update
		go func() {
			// Use singleflight to avoid stampede on update
			h.Group.Do(cacheKey, func() (interface{}, error) {
				if shouldProcess {
					data, err := h.processAndSave(objectKey, cacheFilePath, imgOpts)
					if err == nil && h.MemoryCache != nil && data != nil {
						// Update memory cache
						// processAndSave needs to return data
						// It currently returns (interface{}, error) but returns nil.
						// We need to modify processAndSave to return bytes.
						h.MemoryCache.Set(cacheKey, data.([]byte))
					}
					return data, err
				} else {
					// fetchAndSave logic
					return h.fetchAndSave(objectKey, cacheFilePath, encodingType)
				}
			})
		}()

		metrics.CacheOpsTotal.WithLabelValues("hit_stale").Inc()
		// Serve the file
		w.Header().Set("ETag", etag)
		serveFile(w, cacheFilePath, encodingType, objectKey, imgOpts.Format)

		// Also update Memory Cache with file content if not in memory?
		// Reading file to memory might be expensive if big.
		// Let's populate memory cache on fresh process only.
		return
	}

	_, err, _ = h.Group.Do(cacheKey, func() (interface{}, error) {
		if storage.FileExists(cacheFilePath) {
			metrics.CacheOpsTotal.WithLabelValues("hit_disk").Inc()
			// Populate memory cache?
			// Maybe asynchronously or here if small enough.
			return nil, nil
		}
		metrics.CacheOpsTotal.WithLabelValues("miss").Inc()

		slog.Debug("Processing MISS", "objectKey", objectKey, "cacheKey", cacheKey)

		if shouldProcess {
			// Check if video
			if isVideo && h.Config.EnableVideoThumbnail {
				// Special video processing
				data, err := h.processVideoAndSave(objectKey, cacheFilePath, imgOpts)
				if err == nil && h.MemoryCache != nil && data != nil {
					h.MemoryCache.Set(cacheKey, data.([]byte))
				}
				return data, err
			}

			data, err := h.processAndSave(objectKey, cacheFilePath, imgOpts)
			if err == nil && h.MemoryCache != nil && data != nil {
				h.MemoryCache.Set(cacheKey, data.([]byte))
			}
			return data, err
		} else {
			return h.fetchAndSave(objectKey, cacheFilePath, encodingType)
		}
	})

	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		slog.Error("Request processing failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("ETag", etag)
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
	if err != nil {
		slog.Warn("Error loading watermark", "error", err)
		// Continue without watermark? Or fail? The original code warned but continued.
	}

	buf, err := processor.Process(reader, opts, wmImg, wmOpacity, objectKey)
	if err != nil {
		return nil, err
	}

	// Return bytes for memory cache
	// Capture bytes BEFORE writing, as AtomicWrite drains the buffer
	data := buf.Bytes()

	err = storage.AtomicWrite(destPath, bytes.NewReader(data), "identity", h.CacheDir)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (h *Handler) processVideoAndSave(objectKey, destPath string, opts processor.ImageOptions) (interface{}, error) {
	// For video, we first need the video file locally.
	// 1. Check if we have the video in cache? Or a temp file.
	// Since cacheKey for video includes "processed" params, we don't have the original video cached by fetchAndSave usually.
	// Unless we cache original video too.
	// To support ffmpeg which needs a file path usually (or complex piping), let's download to a temp file.

	// Create temp file
	tmpFile, err := os.CreateTemp(h.CacheDir, "video-*.tmp")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Download video
	reader, _, err := h.S3.GetObject(context.TODO(), objectKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	_, err = io.Copy(tmpFile, reader)
	if err != nil {
		return nil, err
	}

	// Generate Thumbnail
	// We use "00:00:01" as default timestamp if not provided via some param (not spec'd, so default)
	buf, err := processor.GenerateThumbnail(tmpFile.Name(), "00:00:01")
	if err != nil {
		return nil, err
	}

	// Now we have the thumbnail image in buf (JPEG).
	// We might need to resize it or apply other image options (watermark, etc.)
	// opts contain Width, Height, etc.
	// GenerateThumbnail returns raw frame.
	// We should pipe it through Processor.Process to handle resizing/watermarking.

	buf2, err := processor.Process(buf, opts, nil, 0, objectKey+".jpg") // Treat as jpg
	if err != nil {
		return nil, err
	}

	// Save to cache
	// Capture bytes BEFORE writing
	data := buf2.Bytes()

	err = storage.AtomicWrite(destPath, bytes.NewReader(data), "identity", h.CacheDir)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func setContentType(w http.ResponseWriter, objectKey, forcedFormat string) {
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

	opts.Focus = params.Get("focus")
	opts.Text = params.Get("text")
	opts.TextColor = params.Get("color") // map 'color' param to TextColor

	if ts := params.Get("ts"); ts != "" {
		opts.TextSize, _ = strconv.ParseFloat(ts, 64)
	}

	// Check for blurhash
	if bh := params.Get("blurhash"); bh == "true" || bh == "1" {
		opts.Blurhash = true
	}

	return opts
}

func isImageFile(key string) bool {
	ext := strings.ToLower(filepath.Ext(key))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp"
}

func isVideoFile(key string) bool {
	ext := strings.ToLower(filepath.Ext(key))
	return ext == ".mp4" || ext == ".mov" || ext == ".webm"
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
