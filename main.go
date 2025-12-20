package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"

	"github.com/CodeTease/quirm/pkg/cache"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/handlers"
	"github.com/CodeTease/quirm/pkg/metrics"
	"github.com/CodeTease/quirm/pkg/processor"
	"github.com/CodeTease/quirm/pkg/storage"
	"github.com/CodeTease/quirm/pkg/watermark"
)

var (
	Version = "0.3.0"
)

func main() {
	cfg := config.LoadConfig()

	if cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		log.Fatal("Fatal: Missing required S3 configuration.")
	}

	if _, err := os.Stat(cfg.CacheDir); os.IsNotExist(err) {
		os.MkdirAll(cfg.CacheDir, 0755)
	}

	// Initialize components
	if cfg.FaceFinderPath != "" {
		if err := processor.LoadCascade(cfg.FaceFinderPath); err != nil {
			log.Printf("Warning: Failed to load facefinder cascade: %v. Face detection will be disabled.", err)
		}
	}

	wmManager := watermark.NewManager(cfg.WatermarkPath, cfg.WatermarkOpacity, cfg.Debug)

	go cache.StartCleaner(cfg.CacheDir, cfg.CacheTTL, cfg.CleanupInterval, cfg.Debug)

	s3Client, err := storage.NewS3Client(cfg)
	if err != nil {
		log.Fatalf("Fatal: Failed to load AWS config: %v", err)
	}

	requestGroup := &singleflight.Group{}

	// Initialize caches
	memoryCache := cache.NewMemoryCache(100, cfg.CacheTTL) // 100 items limit for memory cache for now

	h := &handlers.Handler{
		Config:      cfg,
		S3:          s3Client,
		WM:          wmManager,
		Group:       requestGroup,
		CacheDir:    cfg.CacheDir,
		MemoryCache: memoryCache,
	}

	// Initialize ipLimiters map
	// Use expirable LRU to avoid memory leak
	// Size 10000, TTL 1 hour
	ipLimiters := expirable.NewLRU[string, *rate.Limiter](10000, nil, time.Hour)
	h.SetIPLimiters(ipLimiters)

	if cfg.EnableMetrics {
		metrics.Init()
		http.Handle("/metrics", promhttp.Handler())
		fmt.Printf("Metrics enabled at /metrics\n")
	}

	http.HandleFunc("/", h.HandleRequest)
	fmt.Printf("Quirm v%s running on port %s\n", Version, cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, nil))
}
