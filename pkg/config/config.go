package config

import (
	"os"
	"strconv"
	"time"

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
	CacheTTL          time.Duration
	CleanupInterval   time.Duration
	Debug             bool
	// New Configs
	SecretKey        string
	WatermarkPath    string
	WatermarkOpacity float64
	MaxImageSizeMB   int64
	EnableMetrics    bool
}

// LoadConfig loads configuration from environment variables
func LoadConfig() Config {
	godotenv.Load()

	return Config{
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
		MaxImageSizeMB:    int64(getEnvInt("MAX_IMAGE_SIZE_MB", 20)),
		EnableMetrics:     getEnvBool("ENABLE_METRICS", false),
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
