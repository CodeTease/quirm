package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"

	"time"

	"golang.org/x/sync/singleflight"

	"github.com/CodeTease/quirm/pkg/cache"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/handlers"
	"github.com/CodeTease/quirm/pkg/logger"
	"github.com/CodeTease/quirm/pkg/metrics"
	"github.com/CodeTease/quirm/pkg/processor"
	"github.com/CodeTease/quirm/pkg/ratelimit"
	"github.com/CodeTease/quirm/pkg/storage"
	"github.com/CodeTease/quirm/pkg/watermark"
	"github.com/davidbyttow/govips/v2/vips"
)

var (
	Version = "0.4.0"
)

func main() {
	// Initialize libvips
	vips.Startup(nil)
	defer vips.Shutdown()

	cfg := config.LoadConfig()
	logger.Init(cfg.Debug)

	if cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		slog.Error("Fatal: Missing required S3 configuration.")
		os.Exit(1)
	}

	if _, err := os.Stat(cfg.CacheDir); os.IsNotExist(err) {
		os.MkdirAll(cfg.CacheDir, 0755)
	}

	// Initialize components
	if cfg.FaceFinderPath != "" {
		if err := processor.LoadCascade(cfg.FaceFinderPath); err != nil {
			slog.Warn("Failed to load facefinder cascade. Face detection will be disabled.", "error", err)
		}
	}

	wmManager := watermark.NewManager(cfg.WatermarkPath, cfg.WatermarkOpacity, cfg.Debug)

	// Hard TTL for cleaner is 7 days (or 7x CacheTTL if simpler, but user said "don't delete immediately")
	// Let's use a reasonably long hard TTL like 7 days or 24 * CacheTTL
	hardTTL := cfg.CacheTTL * 24
	if hardTTL < 24*time.Hour {
		hardTTL = 7 * 24 * time.Hour
	}
	go cache.StartCleaner(cfg.CacheDir, hardTTL, cfg.CleanupInterval, cfg.Debug)

	s3Client, err := storage.NewS3Client(cfg)
	if err != nil {
		slog.Error("Fatal: Failed to load AWS config", "error", err)
		os.Exit(1)
	}

	requestGroup := &singleflight.Group{}

	// Initialize caches
	var cacheProvider cache.CacheProvider
	memoryCache := cache.NewMemoryCache(100, cfg.CacheTTL) // 100 items limit for memory cache for now

	if cfg.RedisAddr != "" {
		redisCache := cache.NewRedisCache(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
		cacheProvider = cache.NewTieredCache(memoryCache, redisCache)
		slog.Info("Initialized Tiered Cache (Memory + Redis)")
	} else {
		cacheProvider = memoryCache
		slog.Info("Initialized Memory Cache")
	}

	// Initialize Rate Limiter
	var limiter ratelimit.Limiter
	if cfg.RateLimit > 0 {
		if cfg.RedisAddr != "" {
			limiter = ratelimit.NewRedisLimiter(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, cfg.RateLimit)
			slog.Info("Initialized Redis Rate Limiter")
		} else {
			limiter = ratelimit.NewMemoryLimiter(cfg.RateLimit, 10000, time.Hour)
			slog.Info("Initialized Memory Rate Limiter")
		}
	}

	// Compile AllowedDomains Regex
	var allowedDomainsRegex []*regexp.Regexp
	for _, d := range cfg.AllowedDomains {
		if strings.HasPrefix(d, "^") {
			re, err := regexp.Compile(d)
			if err != nil {
				slog.Error("Invalid regex in allowed domains", "regex", d, "error", err)
				continue
			}
			allowedDomainsRegex = append(allowedDomainsRegex, re)
		}
	}

	h := &handlers.Handler{
		Config:              cfg,
		S3:                  s3Client,
		WM:                  wmManager,
		Group:               requestGroup,
		CacheDir:            cfg.CacheDir,
		Cache:               cacheProvider,
		Limiter:             limiter,
		AllowedDomainsRegex: allowedDomainsRegex,
	}

	if cfg.EnableMetrics {
		metrics.Init()
		http.Handle("/metrics", promhttp.Handler())
		fmt.Printf("Metrics enabled at /metrics\n")
	}

	http.HandleFunc("/", h.HandleRequest)
	slog.Info("Quirm running", "version", Version, "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, nil); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
