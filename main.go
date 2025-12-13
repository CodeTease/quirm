package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"

	// "image/png" // Unused directly, but might be needed for init side effects? No, imaging handles it.
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	"github.com/joho/godotenv"
	"golang.org/x/sync/singleflight"
)

// Config holds application configuration
type Config struct {
	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKey       string
	S3SecretKey       string
	S3ForcePathStyle  bool
	S3UseCustomDomain bool
	Port              string
	CacheDir          string
	CacheTTL          time.Duration
	CleanupInterval   time.Duration
	Debug             bool
	// New Configs
	SecretKey        string
	WatermarkPath    string
	WatermarkOpacity float64
}

// ImageOptions holds parameters for image processing
type ImageOptions struct {
	Width   int
	Height  int
	Fit     string // cover, contain, fill, inside
	Format  string // jpeg, png, webp
	Quality int
}

var (
	s3Client     *s3.Client
	cfg          Config
	requestGroup singleflight.Group
	watermarkImg image.Image
	Version      = "0.1.0"
)

func main() {
	godotenv.Load()

	cfg = Config{
		S3Endpoint:        os.Getenv("S3_ENDPOINT"),
		S3Region:          getEnv("S3_REGION", "auto"),
		S3Bucket:          os.Getenv("S3_BUCKET"),
		S3AccessKey:       os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:       os.Getenv("S3_SECRET_KEY"),
		S3ForcePathStyle:  getEnvBool("S3_FORCE_PATH_STYLE", false),
		S3UseCustomDomain: getEnvBool("S3_USE_CUSTOM_DOMAIN", false),
		Port:              getEnv("PORT", "8080"),
		CacheDir:          getEnv("CACHE_DIR", "./cache_data"),
		CacheTTL:          time.Duration(getEnvInt("CACHE_TTL_HOURS", 24)) * time.Hour,
		CleanupInterval:   time.Duration(getEnvInt("CLEANUP_INTERVAL_MINS", 60)) * time.Minute,
		Debug:             getEnvBool("DEBUG", false),
		SecretKey:         os.Getenv("SECRET_KEY"),
		WatermarkPath:     os.Getenv("WATERMARK_PATH"),
		WatermarkOpacity:  getEnvFloat("WATERMARK_OPACITY", 0.5),
	}

	if cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		log.Fatal("Fatal: Missing required S3 configuration.")
	}

	if _, err := os.Stat(cfg.CacheDir); os.IsNotExist(err) {
		os.MkdirAll(cfg.CacheDir, 0755)
	}

	// Load Watermark
	if cfg.WatermarkPath != "" {
		var err error
		watermarkImg, err = imaging.Open(cfg.WatermarkPath)
		if err != nil {
			log.Printf("Warning: Failed to load watermark from %s: %v", cfg.WatermarkPath, err)
		} else {
			if cfg.Debug {
				log.Println("Watermark loaded successfully.")
			}
		}
	}

	go startCacheCleaner()

	// AWS Setup
	clientLogMode := aws.LogRequest
	if !cfg.Debug {
		clientLogMode = aws.ClientLogMode(0)
	}

	awsCfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(cfg.S3Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")),
		config.WithClientLogMode(clientLogMode),
	)
	if err != nil {
		log.Fatalf("Fatal: Failed to load AWS config: %v", err)
	}

	s3Client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
		o.UsePathStyle = cfg.S3ForcePathStyle
		if cfg.S3UseCustomDomain {
			o.EndpointResolver = s3.EndpointResolverFunc(func(region string, options s3.EndpointResolverOptions) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:               cfg.S3Endpoint,
					HostnameImmutable: true,
					SigningRegion:     cfg.S3Region,
					Source:            aws.EndpointSourceCustom,
				}, nil
			})
			o.APIOptions = []func(*middleware.Stack) error{
				func(stack *middleware.Stack) error {
					return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("StripBucketFromPath",
						func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
							middleware.FinalizeOutput, middleware.Metadata, error,
						) {
							req, ok := in.Request.(*smithyhttp.Request)
							if !ok {
								return next.HandleFinalize(ctx, in)
							}
							prefix := "/" + cfg.S3Bucket
							if strings.HasPrefix(req.URL.Path, prefix) {
								req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
							}
							return next.HandleFinalize(ctx, in)
						}),
						middleware.Before,
					)
				},
			}
		}
	})

	http.HandleFunc("/", handleRequest)
	fmt.Printf("Quirm v%s running on port %s\n", Version, cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, nil))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
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

	// 2. Parse Image Options
	imgOpts := parseImageOptions(queryParams)

	// Determine Mode
	isImage := isImageFile(objectKey)

	// Auto-Format Logic: Check Accept Header
	// Only apply if user hasn't explicitly requested a format and it is an image
	if isImage && imgOpts.Format == "" {
		acceptHeader := r.Header.Get("Accept")
		if strings.Contains(acceptHeader, "image/webp") {
			imgOpts.Format = "webp"
		}
	}

	// We process if params are present OR if we decided to change format (e.g., auto-webp)
	shouldProcess := isImage && (imgOpts.Width > 0 || imgOpts.Height > 0 || imgOpts.Fit != "" || imgOpts.Format != "")

	cacheKey := ""
	encodingType := "identity"

	if shouldProcess {
		cacheKey = generateCacheKeyProcessed(objectKey, queryParams, imgOpts.Format)
	} else {
		// Passthrough Mode
		acceptEncoding := r.Header.Get("Accept-Encoding")
		if strings.Contains(acceptEncoding, "br") {
			encodingType = "br"
		} else if strings.Contains(acceptEncoding, "gzip") {
			encodingType = "gzip"
		}
		cacheKey = generateCacheKeyOriginal(objectKey, encodingType)
	}

	cacheFilePath := filepath.Join(cfg.CacheDir, cacheKey)

	_, err, _ := requestGroup.Do(cacheKey, func() (interface{}, error) {
		if fileExists(cacheFilePath) {
			return nil, nil
		}
		if cfg.Debug {
			log.Printf("[MISS] Processing: %s (Key: %s)", objectKey, cacheKey)
		}

		if shouldProcess {
			return processAndSave(objectKey, cacheFilePath, imgOpts)
		} else {
			return fetchAndSave(objectKey, cacheFilePath, encodingType)
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

func fetchAndSave(objectKey, destPath, encodingType string) (interface{}, error) {
	resp, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(cfg.S3Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return atomicWrite(destPath, resp.Body, encodingType)
}

func processAndSave(objectKey, destPath string, opts ImageOptions) (interface{}, error) {
	resp, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(cfg.S3Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 1. Decode
	img, err := imaging.Decode(resp.Body)
	if err != nil {
		// If decoding fails, it might not be a supported image.
		return nil, fmt.Errorf("decode error: %w", err)
	}

	// 2. Transform
	if opts.Width > 0 || opts.Height > 0 {
		switch opts.Fit {
		case "cover":
			img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
		case "contain": // Fit
			img = imaging.Fit(img, opts.Width, opts.Height, imaging.Lanczos)
		default: // Resize (stretch/distort? No, imaging.Resize preserves aspect ratio if one dim is 0)
			img = imaging.Resize(img, opts.Width, opts.Height, imaging.Lanczos)
		}
	}

	// 3. Watermark
	if watermarkImg != nil {
		// Calculate position (Bottom Right with padding)
		b := img.Bounds()
		wb := watermarkImg.Bounds()

		// Optional: Scale watermark to fit if it's too big?
		// For now, simple overlay at bottom right.
		offset := image.Pt(b.Max.X-wb.Max.X-10, b.Max.Y-wb.Max.Y-10)
		if offset.X < 0 {
			offset.X = 0
		}
		if offset.Y < 0 {
			offset.Y = 0
		}

		img = imaging.Overlay(img, watermarkImg, offset, cfg.WatermarkOpacity)
	}

	// 4. Encode
	buf := new(bytes.Buffer)

	formatStr := strings.ToLower(opts.Format)
	if formatStr == "" {
		// Keep original format extension if possible, or default to JPEG
		ext := strings.ToLower(filepath.Ext(objectKey))
		if ext == ".png" {
			formatStr = "png"
		} else if ext == ".gif" {
			formatStr = "gif"
		} else {
			formatStr = "jpeg"
		}
	}

	var encodeErr error
	quality := opts.Quality
	if quality == 0 {
		quality = 80
	}

	switch formatStr {
	case "png":
		encodeErr = imaging.Encode(buf, img, imaging.PNG)
	case "gif":
		encodeErr = imaging.Encode(buf, img, imaging.GIF)
	case "webp":
		// Using github.com/chai2010/webp for encoding
		encodeErr = webp.Encode(buf, img, &webp.Options{Quality: float32(quality)})
	default: // jpeg
		encodeErr = jpeg.Encode(buf, img, &jpeg.Options{Quality: quality})
	}

	if encodeErr != nil {
		return nil, encodeErr
	}

	return atomicWrite(destPath, buf, "identity")
}

func atomicWrite(destPath string, r io.Reader, encodingType string) (interface{}, error) {
	tempFile, err := os.CreateTemp(cfg.CacheDir, "quirm_tmp_*")
	if err != nil {
		return nil, err
	}
	tempName := tempFile.Name()

	defer func() {
		tempFile.Close()
		os.Remove(tempName)
	}()

	switch encodingType {
	case "br":
		brWriter := brotli.NewWriterLevel(tempFile, brotli.BestCompression)
		_, err = io.Copy(brWriter, r)
		brWriter.Close()
	case "gzip":
		gzWriter := gzip.NewWriter(tempFile)
		_, err = io.Copy(gzWriter, r)
		gzWriter.Close()
	default:
		_, err = io.Copy(tempFile, r)
	}

	if err != nil {
		return nil, err
	}
	tempFile.Close()

	if fileExists(destPath) {
		os.Remove(destPath)
	}
	if err := os.Rename(tempName, destPath); err != nil {
		return nil, err
	}
	now := time.Now()
	os.Chtimes(destPath, now, now)

	return nil, nil
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

func startCacheCleaner() {
	ticker := time.NewTicker(cfg.CleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		if cfg.Debug {
			log.Println("[CLEANUP] Starting cache cleanup...")
		}
		files, err := os.ReadDir(cfg.CacheDir)
		if err != nil {
			log.Printf("[CLEANUP] Error reading dir: %v", err)
			continue
		}
		deletedCount := 0
		for _, file := range files {
			info, err := file.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > cfg.CacheTTL {
				path := filepath.Join(cfg.CacheDir, file.Name())
				if err := os.Remove(path); err == nil {
					deletedCount++
				}
			}
		}
		if cfg.Debug && deletedCount > 0 {
			log.Printf("[CLEANUP] Removed %d stale files.", deletedCount)
		}
	}
}

// Helpers
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
func getEnvBool(key string, fallback bool) bool {
	if value, ok := os.LookupEnv(key); ok {
		val, err := strconv.ParseBool(value)
		if err == nil {
			return val
		}
	}
	return fallback
}
func getEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		val, err := strconv.Atoi(value)
		if err == nil {
			return val
		}
	}
	return fallback
}
func getEnvFloat(key string, fallback float64) float64 {
	if value, ok := os.LookupEnv(key); ok {
		val, err := strconv.ParseFloat(value, 64)
		if err == nil {
			return val
		}
	}
	return fallback
}
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func isImageFile(key string) bool {
	ext := strings.ToLower(filepath.Ext(key))
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" || ext == ".webp"
}

func generateCacheKeyOriginal(key, encoding string) string {
	h := sha256.New()
	h.Write([]byte(key + encoding))
	return hex.EncodeToString(h.Sum(nil))
}

func generateCacheKeyProcessed(key string, params url.Values, format string) string {
	// Sort params for determinism
	keys := make([]string, 0, len(params))
	for k := range params {
		if k == "s" {
			continue
		} // Ignore signature in cache key
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte(key))
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte(params.Get(k)))
	}
	h.Write([]byte(format))
	return hex.EncodeToString(h.Sum(nil))
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

func parseImageOptions(params url.Values) ImageOptions {
	opts := ImageOptions{}
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
