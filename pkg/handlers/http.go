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
	Config              config.Config
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

		check := func(val string) bool {
			if val == "" {
				return false
			}
			u, err := url.Parse(val)
			if err != nil {
				return false
			}
			// Check exact/wildcard domains first
			for _, d := range h.Config.AllowedDomains {
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
				allowed = true
			}
		}
		if origin != "" {
			if check(origin) {
				allowed = true
			}
		}

		if referer == "" && origin == "" {
			allowed = true
		}

		if !allowed && (referer != "" || origin != "") {
			http.Error(w, "Forbidden Domain", http.StatusForbidden)
			return
		}
	}

	// 0.2 Security: GeoIP
	if len(h.Config.AllowedCountries) > 0 {
		country := r.Header.Get("CF-IPCountry")
		if country == "" {
			country = r.Header.Get("X-Country-Code")
		}
		
		if country != "" {
			allowed := false
			for _, c := range h.Config.AllowedCountries {
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
		// If header missing, we default allow or block? usually allow if not behind proxy, or assume strictly blocked?
		// User said "chỉ cho phép IP Việt Nam chẳng hạn", implies restrictive.
		// But without proxy headers we can't know. Let's assume we block if header IS present and doesn't match, 
		// but if missing, we can't enforce (unless we mandate proxy usage).
		// For safety, let's only block if we KNOW the country and it's wrong.
	}

	// 0.5 Security: Rate Limiting
	// Get IP
	ip := r.RemoteAddr
	// RemoteAddr contains port, strip it
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}

	if h.Config.RateLimit > 0 && h.Limiter != nil {
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

	// 0.6 Feature: Purge Cache
	// We require signature verification (handled above) or a specific admin header if signatures aren't used.
	// For simplicity in this context, we rely on the signature verification above if enabled.
	// If SecretKey is not set, purge is open (like the rest of the app).
	if r.Method == http.MethodDelete {
		h.handlePurge(w, r, objectKey, queryParams)
		return
	}

	// 2. Parse Image Options
	imgOpts := parseImageOptions(queryParams, h.Config.Presets)

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

	// Memory/Redis Cache Check
	if h.Cache != nil {
		if data, found := h.Cache.Get(context.TODO(), cacheKey); found {
			metrics.CacheOpsTotal.WithLabelValues("hit_cache").Inc()
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

	// Check if we should serve stale content
	if fileExists {
		// If file is older than CacheTTL, we serve it but trigger update
		if time.Since(fileInfo.ModTime()) > h.Config.CacheTTL {
			// Trigger background update
			go func() {
				// Use singleflight to avoid stampede on update
				_, _, _ = h.Group.Do(cacheKey, func() (interface{}, error) {
					return h.updateCache(context.Background(), objectKey, cacheFilePath, cacheKey, imgOpts, encodingType, shouldProcess, isVideo)
				})
			}()

			metrics.CacheOpsTotal.WithLabelValues("hit_stale").Inc()
			// Serve the file
			w.Header().Set("ETag", etag)
			serveFile(w, cacheFilePath, encodingType, objectKey, imgOpts.Format)
			return
		}
		
		// File exists and is fresh
		metrics.CacheOpsTotal.WithLabelValues("hit_disk").Inc()
		w.Header().Set("ETag", etag)
		serveFile(w, cacheFilePath, encodingType, objectKey, imgOpts.Format)
		return
	}

	_, err, _ = h.Group.Do(cacheKey, func() (interface{}, error) {
		// Double check inside singleflight
		if storage.FileExists(cacheFilePath) {
			// If it appeared while waiting
			metrics.CacheOpsTotal.WithLabelValues("hit_disk").Inc()
			return nil, nil
		}
		metrics.CacheOpsTotal.WithLabelValues("miss").Inc()

		slog.Debug("Processing MISS", "objectKey", objectKey, "cacheKey", cacheKey)
		return h.updateCache(context.Background(), objectKey, cacheFilePath, cacheKey, imgOpts, encodingType, shouldProcess, isVideo)
	})

	if err != nil {
		// Feature: Fallback/Default Image
		if h.Config.DefaultImagePath != "" {
			if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NoSuchKey") {
				// We serve the default image
				// Note: We might want to cache this response or just serve it directly.
				// Serving directly for simplicity.
				http.ServeFile(w, r, h.Config.DefaultImagePath)
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

func (h *Handler) updateCache(ctx context.Context, objectKey, destPath, cacheKey string, opts processor.ImageOptions, encodingType string, shouldProcess, isVideo bool) ([]byte, error) {
	if shouldProcess {
		if isVideo && h.Config.EnableVideoThumbnail {
			data, err := h.processVideoAndSave(objectKey, destPath, opts)
			if err == nil && h.Cache != nil && len(data) > 0 {
				h.Cache.Set(ctx, cacheKey, data, h.Config.CacheTTL)
			}
			return data, err
		}

		data, err := h.processAndSave(objectKey, destPath, opts)
		if err == nil && h.Cache != nil && len(data) > 0 {
			h.Cache.Set(ctx, cacheKey, data, h.Config.CacheTTL)
		}
		return data, err
	}
	return h.fetchAndSave(objectKey, destPath, encodingType)
}

func (h *Handler) fetchAndSave(objectKey, destPath, encodingType string) ([]byte, error) {
	reader, _, err := h.S3.GetObject(context.TODO(), objectKey)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	// We don't return bytes for fetchAndSave currently as we don't cache originals in Redis yet
	// to avoid high memory/network usage for large files.
	return nil, storage.AtomicWrite(destPath, reader, encodingType, h.CacheDir)
}

func (h *Handler) processAndSave(objectKey, destPath string, opts processor.ImageOptions) ([]byte, error) {
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

func (h *Handler) handlePurge(w http.ResponseWriter, r *http.Request, objectKey string, params url.Values) {
	// Determine keys to purge
	// If params provided, we generate processed key.
	// If no params, we might want to purge original? But usually we purge processed variants.
	// If "all" param provided?
	
	// Implementation: Purge specific variant based on params
	// Need to parse options to generate key properly
	imgOpts := parseImageOptions(params, h.Config.Presets)
	isImage := isImageFile(objectKey)
	isVideo := isVideoFile(objectKey)
	
	shouldProcess := (isImage && (imgOpts.Width > 0 || imgOpts.Height > 0 || imgOpts.Fit != "" || imgOpts.Format != "" || imgOpts.Blurhash)) || (isVideo && h.Config.EnableVideoThumbnail)
	
	var cacheKey string
	if shouldProcess {
		cacheKey = cache.GenerateKeyProcessed(objectKey, params, imgOpts.Format)
	} else {
		// Passthrough
		// We might need to check encoding too if we want to purge specific encoding?
		// Default to identity or check params?
		// For purge, maybe we purge all encodings? 
		// Simplicity: Purge identity or default
		cacheKey = cache.GenerateKeyOriginal(objectKey, "identity")
	}

	// Delete from Cache Provider (Memory + Redis)
	if h.Cache != nil {
		if err := h.Cache.Delete(context.TODO(), cacheKey); err != nil {
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

func (h *Handler) processVideoAndSave(objectKey, destPath string, opts processor.ImageOptions) ([]byte, error) {
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
		
		buf, err = processor.GenerateAnimatedThumbnail(tmpFile.Name(), "3", opts.Width, opts.Height, targetFormat)
		if err != nil {
			return nil, err
		}
		data = buf.Bytes()
	} else {
		// We use "00:00:01" as default timestamp if not provided via some param (not spec'd, so default)
		buf, err = processor.GenerateThumbnail(tmpFile.Name(), "00:00:01")
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

	// Check for blurhash
	if bh := params.Get("blurhash"); bh == "true" || bh == "1" {
		opts.Blurhash = true
	}

	// Check for animated
	if anim := params.Get("animated"); anim == "true" || anim == "1" {
		opts.Animated = true
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
