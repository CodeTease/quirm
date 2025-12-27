package config

import (
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config holds application configuration
type Config struct {
	// Features
	Presets          map[string]string
	DefaultImagePath string

	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3BackupBucket    string
	S3AccessKey       string
	S3SecretKey       string
	S3ForcePathStyle  bool
	S3UseCustomDomain bool
	Port              string
	CacheDir          string
	CacheTTL          time.Duration
	CleanupInterval   time.Duration
	Debug             bool
	// Memory Cache
	MemoryCacheSize       int
	MemoryCacheLimitBytes int64
	// New Configs
	SecretKey        string
	WatermarkPath    string
	WatermarkOpacity float64
	MaxImageSizeMB   int64
	EnableMetrics    bool
	// Security
	AllowedDomains   []string
	AllowedCountries []string
	RateLimit        int // Requests per second
	// Features
	EnableVideoThumbnail bool
	FaceFinderPath       string
	// Redis
	RedisAddr     string
	RedisPassword string
	RedisDB       int
}

// LoadConfig loads configuration from environment variables
func LoadConfig() Config {
	godotenv.Load()

	return Config{
		RedisAddr:            os.Getenv("REDIS_ADDR"),
		RedisPassword:        os.Getenv("REDIS_PASSWORD"),
		RedisDB:              getEnvInt("REDIS_DB", 0),
		S3Endpoint:           os.Getenv("S3_ENDPOINT"),
		S3Region:             getEnv("S3_REGION", "auto"),
		S3Bucket:             os.Getenv("S3_BUCKET"),
		S3BackupBucket:       os.Getenv("S3_BACKUP_BUCKET"),
		S3AccessKey:          os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:          os.Getenv("S3_SECRET_KEY"),
		S3ForcePathStyle:     getEnvBool("S3_FORCE_PATH_STYLE", false),
		S3UseCustomDomain:    getEnvBool("S3_USE_CUSTOM_DOMAIN", false),
		Port:                 getEnv("PORT", "8080"),
		CacheDir:              getEnv("CACHE_DIR", "./cache_data"),
		CacheTTL:              time.Duration(getEnvInt("CACHE_TTL_HOURS", 24)) * time.Hour,
		CleanupInterval:       time.Duration(getEnvInt("CLEANUP_INTERVAL_MINS", 60)) * time.Minute,
		Debug:                 getEnvBool("DEBUG", false),
		MemoryCacheSize:       getEnvInt("MEMORY_CACHE_SIZE", 100),
		MemoryCacheLimitBytes: int64(getEnvInt("MEMORY_CACHE_LIMIT_BYTES", 0)),
		SecretKey:             os.Getenv("SECRET_KEY"),
		WatermarkPath:        os.Getenv("WATERMARK_PATH"),
		WatermarkOpacity:     getEnvFloat("WATERMARK_OPACITY", 0.5),
		MaxImageSizeMB:       int64(getEnvInt("MAX_IMAGE_SIZE_MB", 20)),
		EnableMetrics:        getEnvBool("ENABLE_METRICS", false),
		AllowedDomains:       getEnvSlice("ALLOWED_DOMAINS"),
		AllowedCountries:     getEnvSlice("ALLOWED_COUNTRIES"),
		RateLimit:            getEnvInt("RATE_LIMIT", 10),
		EnableVideoThumbnail: getEnvBool("ENABLE_VIDEO_THUMBNAIL", false),
		FaceFinderPath:       getEnv("FACE_FINDER_PATH", "facefinder"),
		Presets:              getEnvMap("PRESETS"),
		DefaultImagePath:     os.Getenv("DEFAULT_IMAGE_PATH"),
	}
}

// Helpers
func getEnvMap(key string) map[string]string {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(val), &m); err != nil {
		return nil
	}
	return m
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvSlice(key string) []string {
	if value, ok := os.LookupEnv(key); ok {
		return splitString(value)
	}
	return nil
}

func splitString(s string) []string {
	// Simple split by comma
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
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
