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
