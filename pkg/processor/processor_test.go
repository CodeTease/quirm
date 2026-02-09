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

func createMultiColorPNG() []byte {
	w, h := 100, 100
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{255, 0, 0, 255}
	blue := color.RGBA{0, 0, 255, 255}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.Set(x, y, red)
			} else {
				img.Set(x, y, blue)
			}
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

func TestProcess_Resize_Contain(t *testing.T) {
	// Input 200x100 (2:1 aspect ratio)
	input := createDummyPNG(200, 100)
	// Target 100x100 (1:1 aspect ratio) with Fit: "contain"
	// Should result in 100x50 image to maintain aspect ratio within 100x100 box
	opts := ImageOptions{Width: 100, Height: 100, Fit: "contain"}

	output, err := Process(context.Background(), bytes.NewReader(input), opts, nil, 0, "test.png")
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	// Verify output dimensions
	img, err := vips.NewImageFromBuffer(output.Bytes())
	if err != nil {
		t.Fatalf("Failed to decode output image: %v", err)
	}
	defer img.Close()

	if img.Width() != 100 {
		t.Errorf("Expected width 100, got %d", img.Width())
	}
	if img.Height() != 50 {
		t.Errorf("Expected height 50, got %d", img.Height())
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

func TestProcess_PDFFlattening(t *testing.T) {
	t.Skip("Skipping PDF flattening test: govips does not support PDF export")
}

func TestProcess_FontSanitization(t *testing.T) {
	input := createDummyPNG(100, 100)
	opts := ImageOptions{
		Text: "Hello",
		Font: "Arial; <script>alert(1)</script>", // Malicious font name
	}

	output, err := Process(context.Background(), bytes.NewReader(input), opts, nil, 0, "test.png")
	if err != nil {
		t.Fatalf("Process failed with malicious font name: %v", err)
	}

	// If sanitization works, we should get a valid image
	img, err := vips.NewImageFromBuffer(output.Bytes())
	if err != nil {
		t.Fatalf("Failed to decode output image, possible SVG injection caused corruption: %v", err)
	}
	img.Close()
}

func TestExtractPalette(t *testing.T) {
	input := createMultiColorPNG()

	colors, err := ExtractPalette(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("ExtractPalette failed: %v", err)
	}

	// We expect Red (#ff0000) and Blue (#0000ff)
	// Order depends on frequency (50/50 here), so checking existence
	foundRed := false
	foundBlue := false

	for _, c := range colors {
		if c == "#ff0000" {
			foundRed = true
		}
		if c == "#0000ff" {
			foundBlue = true
		}
	}

	if !foundRed {
		t.Errorf("Expected #ff0000 in palette, got %v", colors)
	}
	if !foundBlue {
		t.Errorf("Expected #0000ff in palette, got %v", colors)
	}
}
