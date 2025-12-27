package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/CodeTease/quirm/pkg/cache"
	"github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/metrics"
	"github.com/CodeTease/quirm/pkg/processor"
	"github.com/CodeTease/quirm/pkg/ratelimit"
	"github.com/CodeTease/quirm/pkg/storage"
	"github.com/CodeTease/quirm/pkg/watermark"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
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
	ConfigManager       *config.Manager
	S3                  storage.StorageProvider
	WM                  *watermark.Manager
	Group               *singleflight.Group
	CacheDir            string
	Cache               cache.CacheProvider
	Limiter             ratelimit.Limiter
	AllowedDomainsRegex []*regexp.Regexp
	mu                  sync.Mutex
}

func (h *Handler) HandleRequest(w http.ResponseWriter, r *http.Request) {
	// Start Tracing Span
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	tracer := otel.Tracer("quirm/http")
	ctx, span := tracer.Start(ctx, "HandleRequest",
		trace.WithAttributes(
			semconv.HTTPMethodKey.String(r.Method),
			semconv.HTTPURLKey.String(r.URL.String()),
			semconv.UserAgentOriginalKey.String(r.UserAgent()),
			attribute.String("client.ip", r.RemoteAddr),
		),
		trace.WithSpanKind(trace.SpanKindServer),
	)
	defer span.End()

	// Update request with context
	r = r.WithContext(ctx)

	// Get current config
	cfg := h.ConfigManager.Get()

	// Wrap writer if metrics enabled
	var rec *statusRecorder
	if cfg.EnableMetrics {
		rec = &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		w = rec
	}

	start := time.Now()
	defer func() {
		if cfg.EnableMetrics {
			duration := time.Since(start).Seconds()
			status := strconv.Itoa(rec.statusCode)
			pathLabel := "/{image}" // Generic placeholder as requested
			metrics.HTTPRequestsTotal.WithLabelValues(r.Method, status, pathLabel).Inc()
			metrics.HTTPRequestDuration.WithLabelValues(r.Method, status, pathLabel).Observe(duration)
		}
		if rec != nil {
			span.SetAttributes(semconv.HTTPStatusCodeKey.Int(rec.statusCode))
		}
	}()

	// 0. Security: IP/CIDR Allowlist
	// If the IP is in the allowed CIDR list, we bypass Domain Whitelisting
	ipAllowed := false
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}

	if len(cfg.AllowedCIDRNets) > 0 {
		parsedIP := net.ParseIP(ip)
		if parsedIP != nil {
			for _, ipNet := range cfg.AllowedCIDRNets {
				if ipNet.Contains(parsedIP) {
					ipAllowed = true
					break
				}
			}
		}
	}
	// Fallback check for exact IPs if any (though usually we use CIDRs)
	// If needed we can check AllowedCIDRs strings too if they weren't valid CIDRs but might be IPs

	// 0.1 Security: Domain Whitelisting
	// Only check if IP is NOT explicitly allowed (and if domains are configured)
	if !ipAllowed && len(cfg.AllowedDomains) > 0 {
		referer := r.Header.Get("Referer")
		origin := r.Header.Get("Origin")
		domainAllowed := false

		check := func(val string) bool {
			if val == "" {
				return false
			}
			u, err := url.Parse(val)
			if err != nil {
				return false
			}
			// Check exact/wildcard domains first
			for _, d := range cfg.AllowedDomains {
				if d == "*" {
					return true
				}
				if !strings.HasPrefix(d, "^") && d == u.Host {
					return true
				}
			}
			// Check Regex
			for _, re := range h.AllowedDomainsRegex {
				if re.MatchString(u.Host) {
					return true
				}
			}
			return false
		}

		if referer != "" {
			if check(referer) {
				domainAllowed = true
			}
		}
		if origin != "" {
			if check(origin) {
				domainAllowed = true
			}
		}

		if referer == "" && origin == "" {
			// If no referer/origin, we usually allow unless strict mode is on.
			// Currently implementation allows it.
			domainAllowed = true
		}

		if !domainAllowed && (referer != "" || origin != "") {
			http.Error(w, "Forbidden Domain", http.StatusForbidden)
			return
		}
	} else if !ipAllowed && len(cfg.AllowedCIDRNets) > 0 && len(cfg.AllowedDomains) == 0 {
		// If only CIDRs are configured and IP didn't match -> Forbidden
		http.Error(w, "Forbidden IP", http.StatusForbidden)
		return
	}

	// 0.2 Security: GeoIP
	if len(cfg.AllowedCountries) > 0 {
		country := r.Header.Get("CF-IPCountry")
		if country == "" {
			country = r.Header.Get("X-Country-Code")
		}
		
		if country != "" {
			allowed := false
			for _, c := range cfg.AllowedCountries {
				if strings.EqualFold(c, country) {
					allowed = true
					break
				}
			}
			if !allowed {
				http.Error(w, "Forbidden Country", http.StatusForbidden)
				return
			}
		}
	}

	// 0.5 Security: Rate Limiting
	// IP is already extracted above

	if cfg.RateLimit > 0 && h.Limiter != nil {
		if !h.Limiter.Allow(ip) {
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
	if cfg.SecretKey != "" && len(queryParams) > 0 {
		sig := queryParams.Get("s")
		if sig == "" {
			http.Error(w, "Missing signature", http.StatusForbidden)
			return
		}
		if !validateSignature(r.URL.Path, queryParams, cfg.SecretKey) {
			http.Error(w, "Invalid signature", http.StatusForbidden)
			return
		}
	}

	// 0.6 Feature: Purge Cache
	if r.Method == http.MethodDelete {
		h.handlePurge(w, r, objectKey, queryParams)
		return
	}

	// 2. Parse Image Options
	imgOpts := parseImageOptions(queryParams, cfg.Presets)

	// Feature: Color Palette
	if queryParams.Get("palette") == "true" {
		h.handlePalette(w, r, objectKey, queryParams)
		return
	}

	// Determine Mode
	isImage := isImageFile(objectKey)
	isVideo := isVideoFile(objectKey)

	// Video Thumbnail Logic
	if isVideo && cfg.EnableVideoThumbnail {
		if imgOpts.Format == "" {
			imgOpts.Format = "jpeg"
		}
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

	shouldProcess := (isImage && (imgOpts.Width > 0 || imgOpts.Height > 0 || imgOpts.Fit != "" || imgOpts.Format != "" || imgOpts.Blurhash)) || (isVideo && cfg.EnableVideoThumbnail)

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

	// Memory/Redis Cache Check
	if h.Cache != nil {
		if data, found := h.Cache.Get(ctx, cacheKey); found {
			span.AddEvent("Cache Hit")
			metrics.CacheOpsTotal.WithLabelValues("hit_cache").Inc()
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			
			// If blurhash, text/plain
			if imgOpts.Blurhash {
				w.Header().Set("Content-Type", "text/plain")
			} else {
				setContentType(w, objectKey, imgOpts.Format)
			}

			w.Write(data)
			return
		}
	}

	cacheFilePath := filepath.Join(h.CacheDir, cacheKey)

	// Check file existence and age
	fileInfo, err := os.Stat(cacheFilePath)
	fileExists := err == nil

	// Check if we should serve stale content
	if fileExists {
		// If file is older than CacheTTL, we serve it but trigger update
		if time.Since(fileInfo.ModTime()) > cfg.CacheTTL {
			// Trigger background update
			go func() {
				// Create a background context linked to the original trace?
				// Usually background tasks are separate traces or linked.
				// We'll just use Background for now to avoid cancellation issues.
				_, _, _ = h.Group.Do(cacheKey, func() (interface{}, error) {
					return h.updateCache(context.Background(), objectKey, cacheFilePath, cacheKey, imgOpts, encodingType, shouldProcess, isVideo)
				})
			}()

			span.AddEvent("Serve Stale")
			metrics.CacheOpsTotal.WithLabelValues("hit_stale").Inc()
			// Serve the file
			w.Header().Set("ETag", etag)
			serveFile(w, cacheFilePath, encodingType, objectKey, imgOpts.Format)
			return
		}
		
		// File exists and is fresh
		span.AddEvent("Disk Hit")
		metrics.CacheOpsTotal.WithLabelValues("hit_disk").Inc()
		w.Header().Set("ETag", etag)
		serveFile(w, cacheFilePath, encodingType, objectKey, imgOpts.Format)
		return
	}

	span.AddEvent("Cache Miss")
	_, err, _ = h.Group.Do(cacheKey, func() (interface{}, error) {
		// Double check inside singleflight
		if storage.FileExists(cacheFilePath) {
			// If it appeared while waiting
			metrics.CacheOpsTotal.WithLabelValues("hit_disk").Inc()
			return nil, nil
		}
		metrics.CacheOpsTotal.WithLabelValues("miss").Inc()

		slog.Debug("Processing MISS", "objectKey", objectKey, "cacheKey", cacheKey)
		return h.updateCache(ctx, objectKey, cacheFilePath, cacheKey, imgOpts, encodingType, shouldProcess, isVideo)
	})

	if err != nil {
		// Feature: Fallback/Default Image
		if cfg.DefaultImagePath != "" {
			if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NoSuchKey") {
				http.ServeFile(w, r, cfg.DefaultImagePath)
				return
			}
		}

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

func (h *Handler) handlePalette(w http.ResponseWriter, r *http.Request, objectKey string, params url.Values) {
	cacheKey := cache.GenerateKeyProcessed(objectKey, params, "json")

	// Check Cache
	if h.Cache != nil {
		if data, found := h.Cache.Get(r.Context(), cacheKey); found {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(data)
			return
		}
	}

	// Fetch and Process
	// We use singleflight to avoid duplicate processing
	res, err, _ := h.Group.Do(cacheKey, func() (interface{}, error) {
		reader, _, err := h.S3.GetObject(r.Context(), objectKey)
		if err != nil {
			return nil, err
		}
		defer reader.Close()

		colors, err := processor.ExtractPalette(reader)
		if err != nil {
			return nil, err
		}

		resp := map[string]interface{}{
			"colors": colors,
		}

		data, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		return data, nil
	})

	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		slog.Error("Palette extraction failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	
	data := res.([]byte)

	// Save to Cache
	if h.Cache != nil {
		h.Cache.Set(r.Context(), cacheKey, data, h.ConfigManager.Get().CacheTTL)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

func (h *Handler) updateCache(ctx context.Context, objectKey, destPath, cacheKey string, opts processor.ImageOptions, encodingType string, shouldProcess, isVideo bool) ([]byte, error) {
	ctx, span := otel.Tracer("quirm/handler").Start(ctx, "updateCache",
		trace.WithAttributes(attribute.String("objectKey", objectKey), attribute.String("cacheKey", cacheKey)),
	)
	defer span.End()

	cfg := h.ConfigManager.Get()

	if shouldProcess {
		if isVideo && cfg.EnableVideoThumbnail {
			data, err := h.processVideoAndSave(ctx, objectKey, destPath, opts)
			if err == nil && h.Cache != nil && len(data) > 0 {
				h.Cache.Set(ctx, cacheKey, data, cfg.CacheTTL)
			}
			return data, err
		}

		data, err := h.processAndSave(ctx, objectKey, destPath, opts)
		if err == nil && h.Cache != nil && len(data) > 0 {
			h.Cache.Set(ctx, cacheKey, data, cfg.CacheTTL)
		}
		return data, err
	}
	return h.fetchAndSave(ctx, objectKey, destPath, encodingType)
}

func (h *Handler) fetchAndSave(ctx context.Context, objectKey, destPath, encodingType string) ([]byte, error) {
	reader, _, err := h.S3.GetObject(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	// We don't return bytes for fetchAndSave currently as we don't cache originals in Redis yet
	// to avoid high memory/network usage for large files.
	return nil, storage.AtomicWrite(destPath, reader, encodingType, h.CacheDir)
}

func (h *Handler) processAndSave(ctx context.Context, objectKey, destPath string, opts processor.ImageOptions) ([]byte, error) {
	reader, size, err := h.S3.GetObject(ctx, objectKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	cfg := h.ConfigManager.Get()
	if cfg.MaxImageSizeMB > 0 && size > cfg.MaxImageSizeMB*1024*1024 {
		return nil, &FileSizeError{MaxSizeMB: cfg.MaxImageSizeMB}
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

func (h *Handler) handlePurge(w http.ResponseWriter, r *http.Request, objectKey string, params url.Values) {
	// Determine keys to purge
	// If params provided, we generate processed key.
	// If no params, we might want to purge original? But usually we purge processed variants.
	// If "all" param provided?
	
	// Implementation: Purge specific variant based on params
	// Need to parse options to generate key properly
	cfg := h.ConfigManager.Get()
	imgOpts := parseImageOptions(params, cfg.Presets)
	isImage := isImageFile(objectKey)
	isVideo := isVideoFile(objectKey)
	
	shouldProcess := (isImage && (imgOpts.Width > 0 || imgOpts.Height > 0 || imgOpts.Fit != "" || imgOpts.Format != "" || imgOpts.Blurhash)) || (isVideo && cfg.EnableVideoThumbnail)
	
	var cacheKey string
	if shouldProcess {
		cacheKey = cache.GenerateKeyProcessed(objectKey, params, imgOpts.Format)
	} else {
		// Passthrough
		cacheKey = cache.GenerateKeyOriginal(objectKey, "identity")
	}

	// Delete from Cache Provider (Memory + Redis)
	if h.Cache != nil {
		if err := h.Cache.Delete(r.Context(), cacheKey); err != nil {
			slog.Warn("Failed to delete from cache provider", "key", cacheKey, "error", err)
		}
	}
	
	// Delete from Disk
	cacheFilePath := filepath.Join(h.CacheDir, cacheKey)
	if err := os.Remove(cacheFilePath); err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to delete from disk", "path", cacheFilePath, "error", err)
	}
	
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Purged"))
}

func (h *Handler) processVideoAndSave(ctx context.Context, objectKey, destPath string, opts processor.ImageOptions) ([]byte, error) {
	// 1. Try to get Presigned URL
	videoURL, err := h.S3.GetPresignedURL(ctx, objectKey, 15*time.Minute)
	
	// If getting presigned URL fails, or we decide to fallback (logic simplified here)
	// We might fallback to download. But for now, if it's S3Client, it should support it.
	// However, other providers might not.
	// If error, we fallback to download mode.
	
	var inputPath string
	var cleanup func()

	if err == nil && videoURL != "" {
		inputPath = videoURL
		cleanup = func() {} // No cleanup needed for URL
	} else {
		// Fallback: Download to temp file
		// Create temp file
		tmpFile, err := os.CreateTemp(h.CacheDir, "video-*.tmp")
		if err != nil {
			return nil, err
		}
		// Register cleanup
		cleanup = func() {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
		}
		// Ensure cleanup happens if we return early (but we need it to persist for GenerateThumbnail)
		// We defer cleanup at the end of function, which is fine since GenerateThumbnail is synchronous.
		defer cleanup()

		// Download video
		reader, _, err := h.S3.GetObject(ctx, objectKey)
		if err != nil {
			return nil, err
		}
		defer reader.Close()

		_, err = io.Copy(tmpFile, reader)
		if err != nil {
			return nil, err
		}
		inputPath = tmpFile.Name()
	}

	// Generate Thumbnail
	var buf *bytes.Buffer
	var data []byte

	if opts.Animated {
		// Generate Animated Thumbnail (3 seconds)
		// We respect requested Width/Height and Format if "webp" or "gif".
		// If format is not specified or something else, default to GIF for animated requests unless it's WebP.
		targetFormat := "gif"
		if opts.Format == "webp" {
			targetFormat = "webp"
		}
		
		buf, err = processor.GenerateAnimatedThumbnail(inputPath, "3", opts.Width, opts.Height, targetFormat)
		if err != nil {
			return nil, err
		}
		data = buf.Bytes()
	} else {
		// We use "00:00:01" as default timestamp if not provided via some param (not spec'd, so default)
		buf, err = processor.GenerateThumbnail(inputPath, "00:00:01")
		if err != nil {
			return nil, err
		}

		// Now we have the thumbnail image in buf (JPEG).
		// Pipe it through Processor.Process to handle resizing/watermarking.
		buf2, err := processor.Process(buf, opts, nil, 0, objectKey+".jpg") // Treat as jpg
		if err != nil {
			return nil, err
		}
		data = buf2.Bytes()
	}
	
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
	// Check expiry first if present
	if expiresStr := params.Get("expires"); expiresStr != "" {
		expires, err := strconv.ParseInt(expiresStr, 10, 64)
		if err == nil {
			if time.Now().Unix() > expires {
				return false
			}
		} else {
			// Invalid expires format
			return false
		}
	}

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

func parseImageOptions(params url.Values, presets map[string]string) processor.ImageOptions {
	// Feature: Named Presets
	if presetName := params.Get("preset"); presetName != "" && len(presets) > 0 {
		if presetQuery, ok := presets[presetName]; ok {
			// Parse preset values
			presetParams, err := url.ParseQuery(presetQuery)
			if err == nil {
				// Strict Mode: When a preset is used, we ONLY use the preset's parameters.
				// We ignore any other dimensions provided in the original query to prevent overriding.
				return parseImageOptions(presetParams, nil)
			}
		}
	}

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

	// Effects
	opts.Effect = params.Get("effect")
	opts.Font = params.Get("font")

	if b := params.Get("brightness"); b != "" {
		if val, err := strconv.ParseFloat(b, 64); err == nil {
			opts.Brightness = val
		}
	}

	if c := params.Get("contrast"); c != "" {
		if val, err := strconv.ParseFloat(c, 64); err == nil {
			// Convert "20" -> 1.2
			opts.Contrast = 1.0 + (val / 100.0)
		}
	}

	// Check for blurhash
	if bh := params.Get("blurhash"); bh == "true" || bh == "1" {
		opts.Blurhash = true
	}

	// Check for animated
	if anim := params.Get("animated"); anim == "true" || anim == "1" {
		opts.Animated = true
	}

	// Parse Page
	if p := params.Get("page"); p != "" {
		if pageVal, err := strconv.Atoi(p); err == nil && pageVal > 0 {
			opts.Page = pageVal
		}
	}

	return opts
}

func isImageFile(key string) bool {
	ext := strings.ToLower(filepath.Ext(key))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp" || ext == ".pdf"
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
