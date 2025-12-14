package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"golang.org/x/sync/singleflight"

	"github.com/CodeTease/quirm/pkg/cache"
	"github.com/CodeTease/quirm/pkg/config"
	"github.com/CodeTease/quirm/pkg/handlers"
	"github.com/CodeTease/quirm/pkg/storage"
	"github.com/CodeTease/quirm/pkg/watermark"
)

var (
	Version = "0.2.0"
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
	wmManager := watermark.NewManager(cfg.WatermarkPath, cfg.WatermarkOpacity, cfg.Debug)

	go cache.StartCleaner(cfg.CacheDir, cfg.CacheTTL, cfg.CleanupInterval, cfg.Debug)

	s3Client, err := storage.NewS3Client(cfg)
	if err != nil {
		log.Fatalf("Fatal: Failed to load AWS config: %v", err)
	}

	requestGroup := &singleflight.Group{}

	h := &handlers.Handler{
		Config:   cfg,
		S3:       s3Client,
		WM:       wmManager,
		Group:    requestGroup,
		CacheDir: cfg.CacheDir,
	}

	http.HandleFunc("/", h.HandleRequest)
	fmt.Printf("Quirm v%s running on port %s\n", Version, cfg.Port)
	log.Fatal(http.ListenAndServe(":"+cfg.Port, nil))
}
