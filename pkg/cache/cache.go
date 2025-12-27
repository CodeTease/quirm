package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type CacheProvider interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

func GenerateKeyOriginal(key, encoding string) string {
	h := sha256.New()
	h.Write([]byte(key + encoding))
	return hex.EncodeToString(h.Sum(nil))
}

func GenerateKeyProcessed(key string, params url.Values, format string) string {
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

func StartCleaner(dir string, hardTTL, interval time.Duration, debug bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		slog.Debug("[CLEANUP] Starting cache cleanup...")
		files, err := os.ReadDir(dir)
		if err != nil {
			slog.Error("[CLEANUP] Error reading dir", "error", err)
			continue
		}
		deletedCount := 0
		for _, file := range files {
			info, err := file.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > hardTTL {
				path := filepath.Join(dir, file.Name())
				if err := os.Remove(path); err == nil {
					deletedCount++
				}
			}
		}
		if deletedCount > 0 {
			slog.Debug("[CLEANUP] Cleanup finished", "deleted_files", deletedCount)
		}
	}
}
