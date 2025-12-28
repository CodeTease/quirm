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
	Health(ctx context.Context) error
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

func GetCachePath(dir, key string) string {
	if len(key) < 4 {
		return filepath.Join(dir, key)
	}
	return filepath.Join(dir, key[0:2], key[2:4], key)
}

func StartCleaner(dir string, hardTTL, interval time.Duration, debug bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		slog.Debug("[CLEANUP] Starting cache cleanup...")
		deletedCount := 0
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // skip errors
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if time.Since(info.ModTime()) > hardTTL {
				if err := os.Remove(path); err == nil {
					deletedCount++
				}
			}
			return nil
		})

		if err != nil {
			slog.Error("[CLEANUP] Error walking dir", "error", err)
		}

		// Clean empty directories (optional, but good for sharding)
		var dirs []string
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && path != dir {
				dirs = append(dirs, path)
			}
			return nil
		})

		// Sort by length descending to delete deepest first
		sort.Slice(dirs, func(i, j int) bool {
			return len(dirs[i]) > len(dirs[j])
		})

		emptyDirsRemoved := 0
		for _, d := range dirs {
			if err := os.Remove(d); err == nil {
				emptyDirsRemoved++
			}
		}

		if deletedCount > 0 || emptyDirsRemoved > 0 {
			slog.Debug("[CLEANUP] Cleanup finished", "deleted_files", deletedCount, "deleted_dirs", emptyDirsRemoved)
		}
	}
}
