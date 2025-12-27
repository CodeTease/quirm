package storage

import (
	"context"
	"io"
)

type StorageProvider interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, error)
}
