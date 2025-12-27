package processor

import (
	"bytes"
	"fmt"
	"os/exec"
	"time"

	"github.com/CodeTease/quirm/pkg/metrics"
)

// GenerateThumbnail generates a thumbnail for a video file using ffmpeg.
// It returns a buffer containing the image data (JPEG).
func GenerateThumbnail(videoURL string, timestamp string) (*bytes.Buffer, error) {
	start := time.Now()
	defer func() {
		metrics.ImageProcessDuration.Observe(time.Since(start).Seconds())
	}()

	// Check if ffmpeg is available (should be done at startup, but for safety)
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", err)
	}

	if timestamp == "" {
		timestamp = "00:00:01"
	}

	// Command: ffmpeg -i <videoURL> -ss <timestamp> -vframes 1 -f image2 -
	cmd := exec.Command("ffmpeg",
		"-i", videoURL,
		"-ss", timestamp,
		"-vframes", "1",
		"-f", "image2",
		"-c:v", "mjpeg",
		"-",
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, fmt.Errorf("ffmpeg error: %v, stderr: %s", err, stderr.String())
	}

	return &stdout, nil
}

// GenerateAnimatedThumbnail generates a 3-second animated GIF thumbnail for a video file using ffmpeg.
// It extracts 3 seconds from the beginning (or timestamp).
func GenerateAnimatedThumbnail(videoURL string, duration string) (*bytes.Buffer, error) {
	start := time.Now()
	defer func() {
		metrics.ImageProcessDuration.Observe(time.Since(start).Seconds())
	}()

	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", err)
	}

	// Default 3 seconds
	if duration == "" {
		duration = "3"
	}

	// Logic: Extract 3 seconds from 00:00:00
	// Use palettegen/paletteuse for better GIF quality
	// Scale to fixed width (e.g., 320) to keep size reasonable, or rely on caller to resize?
	// The plan says "pipe through libvips to compress".
	// But resizing animated GIFs in libvips (via Go) can be complex if we don't load all frames.
	// For robustness, let's output a reasonably sized GIF here. 
	// 320px width is a good default for "thumbnail".
	
	// Command: ffmpeg -ss 00:00:00 -t <duration> -i <videoURL> -vf "fps=10,scale=320:-1:flags=lanczos,split[s0][s1];[s0]palettegen[p];[s1][p]paletteuse" -f gif -
	
	cmd := exec.Command("ffmpeg",
		"-ss", "00:00:00",
		"-t", duration,
		"-i", videoURL,
		"-vf", "fps=10,scale=320:-1:flags=lanczos,split[s0][s1];[s0]palettegen[p];[s1][p]paletteuse",
		"-f", "gif",
		"-",
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, fmt.Errorf("ffmpeg animated error: %v, stderr: %s", err, stderr.String())
	}

	return &stdout, nil
}
