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
	Debug             bool
}

var (
	s3Client *s3.Client
	cfg      Config
	Version  = "0.1.0" // Back to the roots!
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
		CacheDir:          getEnv("CACHE_DIR", "./cache"),
		Debug:             getEnvBool("DEBUG", false),
	}

	// Basic Validation
	if cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		log.Fatal("Fatal: Missing required S3 configuration.")
	}

	if _, err := os.Stat(cfg.CacheDir); os.IsNotExist(err) {
		os.MkdirAll(cfg.CacheDir, 0755)
	}

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
			// 1. Fix Hostname: Prevent SDK from creating bucket.domain.com
			o.EndpointResolver = s3.EndpointResolverFunc(func(region string, options s3.EndpointResolverOptions) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:               cfg.S3Endpoint,
					HostnameImmutable: true,
					SigningRegion:     cfg.S3Region,
					Source:            aws.EndpointSourceCustom,
				}, nil
			})

			// 2. Fix Path: Inject Middleware to strip bucket name from path
			// Using APIOptions is safer and avoids type casting errors in LoadDefaultConfig
			o.APIOptions = []func(*middleware.Stack) error{
				func(stack *middleware.Stack) error {
					return stack.Finalize.Add(middleware.FinalizeMiddlewareFunc("StripBucketFromPath",
						func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (
							middleware.FinalizeOutput, middleware.Metadata, error,
						) {
							// Cast the opaque Request to a concrete HTTP Request
							req, ok := in.Request.(*smithyhttp.Request)
							if !ok {
								// Not an HTTP request? Should not happen in S3, but safety first.
								return next.HandleFinalize(ctx, in)
							}

							// Logic: If path starts with /bucketName, strip it.
							// R2 Custom Domain points directly to bucket, so we want /file.png, NOT /bucket/file.png
							prefix := "/" + cfg.S3Bucket
							if strings.HasPrefix(req.URL.Path, prefix) {
								originalPath := req.URL.Path
								req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)

								if cfg.Debug {
									log.Printf("[Middleware] Path Rewrite: %s -> %s", originalPath, req.URL.Path)
								}
							}

							return next.HandleFinalize(ctx, in)
						}),
						middleware.Before, // Execute this BEFORE signing/sending
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
	objectKey := strings.TrimPrefix(r.URL.Path, "/")
	if objectKey == "" {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Quirm Status: OK"))
		return
	}

	acceptEncoding := r.Header.Get("Accept-Encoding")
	encodingType := "identity"
	if strings.Contains(acceptEncoding, "br") {
		encodingType = "br"
	} else if strings.Contains(acceptEncoding, "gzip") {
		encodingType = "gzip"
	}

	cacheFileName := generateCacheKey(objectKey, encodingType)
	cacheFilePath := filepath.Join(cfg.CacheDir, cacheFileName)

	if fileExists(cacheFilePath) {
		if cfg.Debug {
			log.Printf("[HIT] Serving from cache: %s", objectKey)
		}
		serveFile(w, cacheFilePath, encodingType, objectKey)
		return
	}

	if cfg.Debug {
		log.Printf("[MISS] Fetching: %s", objectKey)
	}

	resp, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(cfg.S3Bucket),
		Key:    aws.String(objectKey),
	})

	if err != nil {
		if cfg.Debug {
			log.Printf("!!! FETCH ERROR !!! Key: %s | Error: %v", objectKey, err)
		}
		http.Error(w, "Not Found on Remote Storage", http.StatusNotFound)
		return
	}
	defer resp.Body.Close()

	outFile, err := os.Create(cacheFilePath)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	switch encodingType {
	case "br":
		brWriter := brotli.NewWriterLevel(outFile, brotli.BestCompression)
		io.Copy(brWriter, resp.Body)
		brWriter.Close()
	case "gzip":
		gzWriter := gzip.NewWriter(outFile)
		io.Copy(gzWriter, resp.Body)
		gzWriter.Close()
	default:
		io.Copy(outFile, resp.Body)
	}
	outFile.Close()

	serveFile(w, cacheFilePath, encodingType, objectKey)
}

func serveFile(w http.ResponseWriter, path string, encoding string, originalName string) {
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	switch encoding {
	case "br":
		w.Header().Set("Content-Encoding", "br")
	case "gzip":
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, &http.Request{}, originalName, time.Now(), file)
}

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
