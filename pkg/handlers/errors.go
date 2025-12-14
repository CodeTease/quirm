package handlers

import "fmt"

type FileSizeError struct {
	MaxSizeMB int64
}

func (e *FileSizeError) Error() string {
	return fmt.Sprintf("file size exceeds limit of %d MB", e.MaxSizeMB)
}
