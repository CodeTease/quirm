package watermark

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Helper to create a dummy image file
func createDummyImageFile(path string, w, h int, c color.Color) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func TestWatermarkManager_HotReload(t *testing.T) {
	// Create temporary directory for watermark
	tmpDir := t.TempDir()
	wmPath := filepath.Join(tmpDir, "watermark.png")

	// 1. Create initial watermark (Red)
	if err := createDummyImageFile(wmPath, 10, 10, color.RGBA{255, 0, 0, 255}); err != nil {
		t.Fatalf("Failed to create initial watermark: %v", err)
	}

	mgr := NewManager(wmPath, 0.5, true)

	// 2. Load first time
	img1, opacity1, err := mgr.Get()
	if err != nil {
		t.Fatalf("Get failed 1st time: %v", err)
	}
	if img1 == nil {
		t.Fatal("Expected image 1, got nil")
	}
	if opacity1 != 0.5 {
		t.Errorf("Expected opacity 0.5, got %f", opacity1)
	}

	// 3. Call Get again without change
	img2, _, err := mgr.Get()
	if err != nil {
		t.Fatalf("Get failed 2nd time: %v", err)
	}
	// Pointers should be identical (same object reused)
	if img1 != img2 {
		t.Error("Expected same image object to be returned when file not changed")
	}

	// 4. Update file (Blue)
	// We need to ensure ModTime changes.
	time.Sleep(1100 * time.Millisecond) // Sleep > 1s to ensure mtime diff on all filesystems

	if err := createDummyImageFile(wmPath, 10, 10, color.RGBA{0, 0, 255, 255}); err != nil {
		t.Fatalf("Failed to update watermark: %v", err)
	}

	// 5. Call Get again -> Should reload
	img3, _, err := mgr.Get()
	if err != nil {
		t.Fatalf("Get failed 3rd time: %v", err)
	}

	if img3 == img1 {
		t.Error("Expected new image object after file update")
	}

	// Check content: img1 is Red, img3 is Blue
	r1, _, _, _ := img1.At(0, 0).RGBA()
	r3, _, _, _ := img3.At(0, 0).RGBA()

	// Red: R high. Blue: R low (0).
	if r1 == r3 {
		t.Errorf("Expected different pixel color (Red vs Blue), got %d vs %d", r1, r3)
	}
}
