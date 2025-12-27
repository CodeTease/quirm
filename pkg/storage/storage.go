package storage

import (
	"context"
	"io"
	"time"
)

type StorageProvider interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
	GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error)
}
