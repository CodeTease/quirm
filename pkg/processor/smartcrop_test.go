package processor

import (
	"errors"
	"image"
	"testing"

	"github.com/davidbyttow/govips/v2/vips"
)

// MockDetector implements ObjectDetector interface for testing
type MockDetector struct {
	ReturnRect *image.Rectangle
	ReturnErr  error
}

func (m *MockDetector) Detect(img *vips.ImageRef) (*image.Rectangle, error) {
	return m.ReturnRect, m.ReturnErr
}

func TestSmartCrop_UsesDetector(t *testing.T) {
	// Create a 100x100 dummy image
	pngData := createDummyPNG(100, 100)
	img, err := vips.NewImageFromBuffer(pngData)
	if err != nil {
		t.Fatalf("Failed to create dummy image: %v", err)
	}
	defer img.Close()

	mock := &MockDetector{
		ReturnRect: &image.Rectangle{Min: image.Point{0, 0}, Max: image.Point{10, 10}},
	}

	// Target 50x50
	err = SmartCrop(img, 50, 50, mock)
	if err != nil {
		t.Fatalf("SmartCrop failed: %v", err)
	}

	// SmartCrop should have cropped to 0,0 10,10 then resized/extended to 50x50
	if img.Width() != 50 {
		t.Errorf("Expected width 50, got %d", img.Width())
	}
	if img.Height() != 50 {
		t.Errorf("Expected height 50, got %d", img.Height())
	}
}

func TestSmartCrop_Fallback(t *testing.T) {
	pngData := createDummyPNG(100, 100)
	img, err := vips.NewImageFromBuffer(pngData)
	if err != nil {
		t.Fatalf("Failed to create dummy image: %v", err)
	}
	defer img.Close()

	// Mock returns nil, nil (simulate no object found)
	mock := &MockDetector{
		ReturnRect: nil,
		ReturnErr:  nil,
	}

	err = SmartCrop(img, 50, 50, mock)
	if err != nil {
		t.Fatalf("SmartCrop failed in fallback: %v", err)
	}
	
	// It should use Entropy crop
	if img.Width() != 50 {
		t.Errorf("Expected width 50, got %d", img.Width())
	}
	if img.Height() != 50 {
		t.Errorf("Expected height 50, got %d", img.Height())
	}
}

func TestSmartCrop_Fallback_OnError(t *testing.T) {
	pngData := createDummyPNG(100, 100)
	img, err := vips.NewImageFromBuffer(pngData)
	if err != nil {
		t.Fatalf("Failed to create dummy image: %v", err)
	}
	defer img.Close()

	// Mock returns error
	mock := &MockDetector{
		ReturnRect: nil,
		ReturnErr:  errors.New("detect error"),
	}

	err = SmartCrop(img, 50, 50, mock)
	if err != nil {
		t.Fatalf("SmartCrop failed in fallback on error: %v", err)
	}
	
	if img.Width() != 50 {
		t.Errorf("Expected width 50, got %d", img.Width())
	}
	if img.Height() != 50 {
		t.Errorf("Expected height 50, got %d", img.Height())
	}
}
