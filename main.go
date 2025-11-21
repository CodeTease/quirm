package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	CacheTTL          time.Duration // Time-to-live for cached files
	CleanupInterval   time.Duration // Interval for the background cleaner
	Debug             bool
}

var (
	s3Client *s3.Client
	cfg      Config
	// requestGroup handles duplicate requests (SingleFlight pattern)
	requestGroup singleflight.Group
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
	}

	// Basic Validation
	if cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		log.Fatal("Fatal: Missing required S3 configuration.")
	}

	if _, err := os.Stat(cfg.CacheDir); os.IsNotExist(err) {
		os.MkdirAll(cfg.CacheDir, 0755)
	}

	// Start Background Cache Cleaner (Garbage Collector)
	go startCacheCleaner()

	// Logging Setup
	clientLogMode := aws.LogRequest
	if !cfg.Debug {
		clientLogMode = aws.ClientLogMode(0)
	}

	// Load AWS Config
	awsCfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(cfg.S3Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")),
		config.WithClientLogMode(clientLogMode),
	)
	if err != nil {
		log.Fatalf("Fatal: Failed to load AWS config: %v", err)
	}

	// Initialize S3 Client with Middleware Injection
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

	fmt.Printf("Quirm v%s (Somewhat Stable) running on port %s\n", Version, cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, nil))
}

func handleRequest(w http.ResponseWriter, r *http.Request) {
	// 1. SECURITY: Path Traversal Protection & Windows Path Fix
	// filepath.Clean converts / to \ on Windows. We force it back to / for S3 keys.
	cleanedPath := filepath.ToSlash(filepath.Clean(r.URL.Path))
	objectKey := strings.TrimPrefix(cleanedPath, "/")

	// Prevent access to system files or hidden files
	if strings.Contains(objectKey, "..") || objectKey == ".env" || objectKey == "" {
		http.Error(w, "Invalid Path", http.StatusBadRequest)
		return
	}

	// Determine Encoding
	acceptEncoding := r.Header.Get("Accept-Encoding")
	encodingType := "identity"
	if strings.Contains(acceptEncoding, "br") {
		encodingType = "br"
	} else if strings.Contains(acceptEncoding, "gzip") {
		encodingType = "gzip"
	}

	// Generate Cache Key
	cacheFileName := generateCacheKey(objectKey, encodingType)
	cacheFilePath := filepath.Join(cfg.CacheDir, cacheFileName)

	// --- SINGLEFLIGHT PATTERN ---
	flightKey := objectKey + "|" + encodingType

	_, err, _ := requestGroup.Do(flightKey, func() (interface{}, error) {
		// Double-checked locking
		if fileExists(cacheFilePath) {
			return nil, nil
		}

		if cfg.Debug {
			log.Printf("[MISS] Fetching from Origin: %s (%s)", objectKey, encodingType)
		}

		return fetchAndSave(objectKey, cacheFilePath, encodingType)
	})

	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		log.Printf("Error processing request: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if cfg.Debug {
		log.Printf("[HIT] Serving: %s", objectKey)
	}
	serveFile(w, cacheFilePath, encodingType, objectKey)
}

// fetchAndSave downloads from S3 and saves to disk using Atomic Write
func fetchAndSave(objectKey, destPath, encodingType string) (interface{}, error) {
	resp, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(cfg.S3Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// ATOMIC WRITE START
	tempFile, err := os.CreateTemp(cfg.CacheDir, "quirm_tmp_*")
	if err != nil {
		return nil, err
	}
	tempName := tempFile.Name()

	// Defer cleanup in case of panic or error before rename
	defer func() {
		// It's safe to close a file multiple times (though returns error, we ignore here)
		tempFile.Close()
		// If rename failed, this removes the temp file. If renamed, this does nothing.
		os.Remove(tempName)
	}()

	// Compression logic
	switch encodingType {
	case "br":
		brWriter := brotli.NewWriterLevel(tempFile, brotli.BestCompression)
		_, err = io.Copy(brWriter, resp.Body)
		brWriter.Close()
	case "gzip":
		gzWriter := gzip.NewWriter(tempFile)
		_, err = io.Copy(gzWriter, resp.Body)
		gzWriter.Close()
	default:
		_, err = io.Copy(tempFile, resp.Body)
	}

	if err != nil {
		return nil, err
	}

	// CRITICAL FIX FOR WINDOWS:
	// We MUST close the file handle explicitly BEFORE renaming.
	// Windows locks the file if it's still open.
	tempFile.Close()

	// On Windows, Rename fails if destPath exists. We should try to remove dest first just in case.
	// (Although SingleFlight prevents race, a stale file might exist)
	if fileExists(destPath) {
		os.Remove(destPath)
	}

	// Atomic Rename
	if err := os.Rename(tempName, destPath); err != nil {
		return nil, err
	}

	// Update ModTime for the cleaner
	now := time.Now()
	os.Chtimes(destPath, now, now)

	return nil, nil
}

func serveFile(w http.ResponseWriter, path string, encoding string, originalName string) {
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

	// Content-Type detection
	ext := filepath.Ext(originalName)
	mimeType := "application/octet-stream"
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
	http.ServeContent(w, &http.Request{}, originalName, time.Now(), file)
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
				// On Windows, this might fail if file is currently being served (Open).
				// We just ignore error and try next time.
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

// --- Helpers ---

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

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func generateCacheKey(key, encoding string) string {
	h := sha256.New()
	h.Write([]byte(key + encoding))
	return hex.EncodeToString(h.Sum(nil))
}
