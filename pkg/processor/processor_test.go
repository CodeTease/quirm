package processor

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
)

func TestMain(m *testing.M) {
	vips.Startup(nil)
	defer vips.Shutdown()
	os.Exit(m.Run())
}

// Helper to create a simple PNG image
func createDummyPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func TestProcess_FormatConversion(t *testing.T) {
	// Setup generic 1x1 PNG buffer
	input := createDummyPNG(1, 1)
	opts := ImageOptions{Format: "webp", Quality: 80}

	// Call Process
	// We pass nil for wmImg and 0 for wmOpacity as they are optional
	output, err := Process(context.Background(), bytes.NewReader(input), opts, nil, 0, "test.png")

	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	// Verify WebP magic bytes (RIFF....WEBP)
	if !bytes.HasPrefix(output.Bytes(), []byte("RIFF")) {
		t.Errorf("Expected RIFF header for WebP")
	}
	// Note: checking specifically for WEBP at offset 8 might be better
	if len(output.Bytes()) > 12 && string(output.Bytes()[8:12]) != "WEBP" {
		t.Errorf("Expected WEBP in header, got %s", string(output.Bytes()[8:12]))
	}
}

func TestProcess_Resize(t *testing.T) {
	input := createDummyPNG(100, 100)
	opts := ImageOptions{Width: 50, Height: 50, Fit: "cover"}
	
	output, err := Process(context.Background(), bytes.NewReader(input), opts, nil, 0, "test.png")
	
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	
	if output.Len() == 0 {
		t.Errorf("Output buffer is empty")
	}
}

func TestProcess_Effects(t *testing.T) {
	input := createDummyPNG(100, 100)
	
	t.Run("Grayscale", func(t *testing.T) {
		opts := ImageOptions{Effect: "grayscale"}
		_, err := Process(context.Background(), bytes.NewReader(input), opts, nil, 0, "test.png")
		if err != nil {
			t.Errorf("Process with grayscale failed: %v", err)
		}
	})
	
	t.Run("Sepia", func(t *testing.T) {
		opts := ImageOptions{Effect: "sepia"}
		_, err := Process(context.Background(), bytes.NewReader(input), opts, nil, 0, "test.png")
		if err != nil {
			t.Errorf("Process with sepia failed: %v", err)
		}
	})
}
