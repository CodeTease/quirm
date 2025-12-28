package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
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
	"github.com/CodeTease/quirm/pkg/telemetry"
	"github.com/CodeTease/quirm/pkg/watermark"
	"github.com/davidbyttow/govips/v2/vips"
)

var (
	Version = "0.4.0"
)

func main() {
	// Setup fonts
	if err := config.SetupFonts(); err != nil {
		fmt.Printf("Warning: Failed to setup fonts: %v\n", err)
	}

	// Initialize libvips
	vips.Startup(nil)
	defer vips.Shutdown()

	cfgManager := config.NewManager()
	cfg := cfgManager.Get()
	logger.Init(cfg.Debug)

	// Initialize Tracing
	shutdownTracer, err := telemetry.InitTracer(context.Background(), "quirm")
	if err != nil {
		slog.Warn("Failed to initialize tracer", "error", err)
	} else {
		defer func() {
			if err := shutdownTracer(context.Background()); err != nil {
				slog.Error("Failed to shutdown tracer", "error", err)
			}
		}()
		slog.Info("Tracing initialized")
	}

	// Listen for SIGHUP to reload config
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGHUP)
		for range c {
			slog.Info("Received SIGHUP, reloading config...")
			if err := cfgManager.Reload(); err != nil {
				slog.Error("Failed to reload config", "error", err)
			} else {
				slog.Info("Config reloaded successfully")
			}
		}
	}()

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

	if cfg.AIModelPath != "" {
		if _, err := os.Stat(cfg.AIModelPath); err != nil {
			slog.Error("Fatal: AI Model configured but file not found.", "path", cfg.AIModelPath, "error", err)
			os.Exit(1)
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
	memoryCache := cache.NewMemoryCache(cfg.MemoryCacheSize, cfg.MemoryCacheLimitBytes, cfg.CacheTTL)

	if cfg.RedisAddr != "" {
		redisAddrs := strings.Split(cfg.RedisAddr, ",")
		redisCache := cache.NewRedisCache(redisAddrs, cfg.RedisPassword, cfg.RedisDB)
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
			redisAddrs := strings.Split(cfg.RedisAddr, ",")
			limiter = ratelimit.NewRedisLimiter(redisAddrs, cfg.RedisPassword, cfg.RedisDB, cfg.RateLimit)
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
		ConfigManager:       cfgManager,
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

	// Health Check
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		status := "ok"
		statusCode := http.StatusOK
		details := make(map[string]string)

		// Check S3
		if err := s3Client.Health(ctx); err != nil {
			status = "error"
			statusCode = http.StatusServiceUnavailable
			details["s3"] = err.Error()
			slog.Error("Health check failed: S3", "error", err)
		} else {
			details["s3"] = "ok"
		}

		// Check Cache (Redis if configured)
		if err := cacheProvider.Health(ctx); err != nil {
			status = "error"
			statusCode = http.StatusServiceUnavailable
			details["cache"] = err.Error()
			slog.Error("Health check failed: Cache", "error", err)
		} else {
			details["cache"] = "ok"
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		fmt.Fprintf(w, `{"status": "%s", "details": %v}`, status, detailsToString(details))
	})

	slog.Info("Quirm running", "version", Version, "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, nil); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
