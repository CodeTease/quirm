package processor

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	"github.com/gen2brain/avif"

	"github.com/CodeTease/quirm/pkg/metrics"
)

type ImageOptions struct {
	Width   int
	Height  int
	Fit     string // cover, contain, fill, inside
	Format  string // jpeg, png, webp
	Quality int
}

// Process decodes, transforms, watermarks, and encodes the image.
// It returns a bytes.Buffer containing the processed image data.
func Process(r io.Reader, opts ImageOptions, wmImg image.Image, wmOpacity float64, originalKey string) (*bytes.Buffer, error) {
	start := time.Now()
	defer func() {
		metrics.ImageProcessDuration.Observe(time.Since(start).Seconds())
	}()

	// 1. Decode
	img, err := imaging.Decode(r)
	if err != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, fmt.Errorf("decode error: %w", err)
	}

	// 2. Transform
	if opts.Width > 0 || opts.Height > 0 {
		switch opts.Fit {
		case "cover":
			img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
		case "contain":
			img = imaging.Fit(img, opts.Width, opts.Height, imaging.Lanczos)
		default: // Resize
			img = imaging.Resize(img, opts.Width, opts.Height, imaging.Lanczos)
		}
	}

	// 3. Watermark
	if wmImg != nil {
		b := img.Bounds()
		wb := wmImg.Bounds()

		offset := image.Pt(b.Max.X-wb.Max.X-10, b.Max.Y-wb.Max.Y-10)
		if offset.X < 0 {
			offset.X = 0
		}
		if offset.Y < 0 {
			offset.Y = 0
		}

		img = imaging.Overlay(img, wmImg, offset, wmOpacity)
	}

	// 4. Encode
	buf := new(bytes.Buffer)

	formatStr := strings.ToLower(opts.Format)
	if formatStr == "" {
		// Keep original format extension if possible, or default to JPEG
		ext := strings.ToLower(filepath.Ext(originalKey))
		if ext == ".png" {
			formatStr = "png"
		} else if ext == ".gif" {
			formatStr = "gif"
		} else {
			formatStr = "jpeg"
		}
	}

	var encodeErr error
	quality := opts.Quality
	if quality == 0 {
		quality = 80
	}

	switch formatStr {
	case "png":
		encodeErr = imaging.Encode(buf, img, imaging.PNG)
	case "gif":
		encodeErr = imaging.Encode(buf, img, imaging.GIF)
	case "webp":
		encodeErr = webp.Encode(buf, img, &webp.Options{Quality: float32(quality)})
	case "avif":
		encodeErr = avif.Encode(buf, img, avif.Options{Quality: quality})
	default: // jpeg
		encodeErr = jpeg.Encode(buf, img, &jpeg.Options{Quality: quality})
	}

	if encodeErr != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, encodeErr
	}

	return buf, nil
}
