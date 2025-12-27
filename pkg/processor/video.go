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

// GenerateAnimatedThumbnail generates a 3-second animated thumbnail for a video file using ffmpeg.
// It extracts 3 seconds from the beginning (or timestamp).
func GenerateAnimatedThumbnail(videoURL string, duration string, width int, height int, format string) (*bytes.Buffer, error) {
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

	// Determine dimensions
	w := "320"
	h := "-1"
	if width > 0 {
		w = fmt.Sprintf("%d", width)
	}
	if height > 0 {
		h = fmt.Sprintf("%d", height)
	}
	scaleFilter := fmt.Sprintf("scale=%s:%s:flags=lanczos", w, h)

	var cmd *exec.Cmd

	if format == "webp" {
		// Animated WebP
		// ffmpeg -ss 00:00:00 -t 3 -i input -vf "fps=10,scale=..." -vcodec libwebp -lossless 0 -compression_level 4 -q:v 75 -loop 0 -preset default -an -vsync 0 -f webp -
		cmd = exec.Command("ffmpeg",
			"-ss", "00:00:00",
			"-t", duration,
			"-i", videoURL,
			"-vf", "fps=10,"+scaleFilter,
			"-vcodec", "libwebp",
			"-lossless", "0",
			"-compression_level", "4",
			"-q:v", "75",
			"-loop", "0",
			"-preset", "default",
			"-an",
			"-f", "webp",
			"-",
		)
	} else {
		// GIF (Default)
		// Use palettegen/paletteuse for better GIF quality
		cmd = exec.Command("ffmpeg",
			"-ss", "00:00:00",
			"-t", duration,
			"-i", videoURL,
			"-vf", fmt.Sprintf("fps=10,%s,split[s0][s1];[s0]palettegen[p];[s1][p]paletteuse", scaleFilter),
			"-f", "gif",
			"-",
		)
	}

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
