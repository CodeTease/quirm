package storage

import (
	"compress/gzip"
	"io"
	"os"
	"time"

	"github.com/andybalholm/brotli"
)

func AtomicWrite(destPath string, r io.Reader, encodingType string, tempDir string) error {
	tempFile, err := os.CreateTemp(tempDir, "quirm_tmp_*")
	if err != nil {
		return err
	}
	tempName := tempFile.Name()

	defer func() {
		tempFile.Close()
		os.Remove(tempName) // Clean up if rename wasn't reached
	}()

	switch encodingType {
	case "br":
		brWriter := brotli.NewWriterLevel(tempFile, brotli.BestCompression)
		_, err = io.Copy(brWriter, r)
		brWriter.Close()
	case "gzip":
		gzWriter := gzip.NewWriter(tempFile)
		_, err = io.Copy(gzWriter, r)
		gzWriter.Close()
	default:
		_, err = io.Copy(tempFile, r)
	}

	if err != nil {
		return err
	}
	tempFile.Close()

	if FileExists(destPath) {
		os.Remove(destPath)
	}
	if err := os.Rename(tempName, destPath); err != nil {
		return err
	}
	now := time.Now()
	os.Chtimes(destPath, now, now)

	return nil
}

func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}
