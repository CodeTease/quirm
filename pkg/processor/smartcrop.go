package processor

import (
	"fmt"
	"image"
	"log/slog"
	"os"
	"runtime"
	"sync"

	"github.com/davidbyttow/govips/v2/vips"
	ort "github.com/yalue/onnxruntime_go"
)

// ObjectDetector defines the interface for object detection logic.
type ObjectDetector interface {
	// Detect returns a crop rectangle focused on the main object.
	// If detection fails or no object found, it returns the input rect or a center rect.
	Detect(img *vips.ImageRef) (*image.Rectangle, error)
}

// EntropyDetector uses Shannon entropy to find the most "interesting" part of the image.
type EntropyDetector struct{}

func (d *EntropyDetector) Detect(img *vips.ImageRef) (*image.Rectangle, error) {
	return nil, nil // Signal to fallback to vips built-in
}

// AiDetector uses ONNX Runtime to detect objects.
// It requires an ONNX model file path (e.g. YOLOv8n) and the ONNX Runtime shared library.
type AiDetector struct {
	ModelPath string
}

var (
	ortInitialized bool
	ortSession     *ort.DynamicAdvancedSession
	ortOnce        sync.Once
	ortError       error
)

func initORT(modelPath string) error {
	ortOnce.Do(func() {
		// Attempt to load shared library from standard paths or a specific env var
		libPath := os.Getenv("ORT_LIB_PATH")
		if libPath != "" {
			ort.SetSharedLibraryPath(libPath)
		}

		if err := ort.InitializeEnvironment(); err != nil {
			ortError = fmt.Errorf("failed to initialize onnx environment: %w", err)
			return
		}
		
		if _, err := os.Stat(modelPath); err != nil {
			ortError = fmt.Errorf("model not found at %s", modelPath)
			return
		}

		// Create Session (Singleton)
		// Allow configuring input/output names via env vars
		inputName := os.Getenv("AI_MODEL_INPUT_NAME")
		if inputName == "" {
			inputName = "images"
		}
		outputName := os.Getenv("AI_MODEL_OUTPUT_NAME")
		if outputName == "" {
			outputName = "output0"
		}

		session, err := ort.NewDynamicAdvancedSession(modelPath,
			[]string{inputName},
			[]string{outputName},
			nil,
		)
		if err != nil {
			ortError = fmt.Errorf("failed to create ONNX session: %w", err)
			return
		}
		ortSession = session
		ortInitialized = true
	})
	return ortError
}

func (d *AiDetector) Detect(img *vips.ImageRef) (*image.Rectangle, error) {
	if d.ModelPath == "" {
		d.ModelPath = os.Getenv("AI_MODEL_PATH")
	}

	if d.ModelPath == "" {
		return nil, nil // No model configured
	}

	// Initialize ONNX Runtime (Singleton)
	if err := initORT(d.ModelPath); err != nil {
		// Log once or debug?
		slog.Debug("AI Detector init failed", "error", err)
		return nil, nil
	}
	
	if ortSession == nil {
		return nil, nil
	}

	// Preprocessing: Resize to 640x640 (standard YOLO)
	// We operate on a copy
	inputImg, err := img.Copy()
	if err != nil {
		return nil, err
	}
	defer inputImg.Close()

	if err := inputImg.ThumbnailWithSize(640, 640, vips.InterestingNone, vips.SizeForce); err != nil {
		return nil, err
	}
	// Force sRGB
	if err := inputImg.ToColorSpace(vips.InterpretationSRGB); err != nil {
		return nil, err
	}
	
	// Ensure 3 bands (Flatten alpha if present)
	if inputImg.Bands() > 3 {
		white := &vips.Color{R: 255, G: 255, B: 255}
		if err := inputImg.Flatten(white); err != nil {
			return nil, err
		}
	}
	// Verify it is now 3 bands
	if inputImg.Bands() != 3 {
		slog.Warn("AI Input has unexpected bands", "bands", inputImg.Bands())
		return nil, nil // Fallback
	}

	// Convert to Tensor [1, 3, 640, 640] float32 normalized 0-1
	width := inputImg.Width()
	height := inputImg.Height()
	data, err := inputImg.ToBytes()
	if err != nil {
		return nil, err
	}

	inputTensorData := make([]float32, 1*3*640*640)
	// Vips export is R G B R G B...
	// YOLO needs RRR... GGG... BBB... (Planar)
	// And normalized 0.0-1.0
	
	for i := 0; i < width*height; i++ {
		r := float32(data[i*3]) / 255.0
		g := float32(data[i*3+1]) / 255.0
		b := float32(data[i*3+2]) / 255.0

		inputTensorData[i] = r
		inputTensorData[width*height + i] = g
		inputTensorData[2*width*height + i] = b
	}

	inputShape := ort.NewShape(1, 3, 640, 640)
	input, err := ort.NewTensor(inputShape, inputTensorData)
	if err != nil {
		return nil, err
	}
	defer input.Destroy()

	// Run Inference
	// RunTensor is not available in all versions, we should use Run() with input/output lists
	// But ortSession.Run() signature is Run(inputs []Value, outputNames []string, outputValues []Value)
	// DynamicAdvancedSession might have Run() which returns outputs.
	// Looking at onnxruntime_go typical usage:
	// outputs, err := session.Run([]Value{input}, []string{outputName}) or similar.
	// Actually, DynamicAdvancedSession.Run() might take input tensor map?
	// Given I cannot browse docs, I will assume the user report "has no field or method RunTensor" implies I used a non-existent method.
	// I will try to use the most generic `Run()` if available, or I will use `RunInputOutput`.
	// Let's assume `Run()` takes list of inputs and returns list of outputs.
	
	// Output is usually [1, 84, 8400] (Classes+Box, Anchors) or similar depending on model export.
	// We will assume [1, 5+, N] where 5+ is x, y, w, h, confidence, class_probs...
	outputShape := ort.NewShape(1, 84, 8400)
	outputDataBuf := make([]float32, 1*84*8400)
	outputTensor, err := ort.NewTensor(outputShape, outputDataBuf)
	if err != nil {
		return nil, err
	}
	defer outputTensor.Destroy()

	err = ortSession.Run([]ort.Value{input}, []ort.Value{outputTensor})
	if err != nil {
		slog.Error("Inference failed", "error", err)
		return nil, err
	}

	// Post-process YOLO output
	// Output is usually [1, 84, 8400] (Classes+Box, Anchors) or similar depending on model export.
	// We will assume [1, 5+, N] where 5+ is x, y, w, h, confidence, class_probs...
	
	// We need to cast Value to Tensor if needed, but GetData() is on the interface.
	// Ensuring we don't have unused variables.
	
	outputDataRaw := outputTensor.GetData()
	dims := outputTensor.GetShape()
	
	if len(dims) < 3 {
		return nil, nil
	}

	// Type assertion
	outputData, ok := outputDataRaw.([]float32)
	if !ok {
		slog.Error("Unexpected tensor data type", "type", fmt.Sprintf("%T", outputDataRaw))
		return nil, nil
	}
	
	// Simply find the anchor with highest objectness/class probability
	// For YOLOv8: [Batch, 4+Classes, Anchors] -> [1, 84, 8400] (80 classes)
	// 0: x center, 1: y center, 2: width, 3: height, 4..: class probs
	
	channels := int(dims[1]) // 84
	anchors := int(dims[2])  // 8400
	
	var bestConf float32 = 0.0
	var bestIdx int = -1
	
	// Iterate over anchors
	for i := 0; i < anchors; i++ {
		// Find max class probability for this anchor
		var maxClassConf float32 = 0.0
		for c := 4; c < channels; c++ {
			// Check bounds
			idx := c*anchors + i
			if idx >= len(outputData) {
				break 
			}
			conf := outputData[idx] 
			// Wait, if shape is [1, 84, 8400], it is contiguous in last dim?
			// Usually data layout in C array: [batch][channel][anchor]
			// So index = c * anchors + i
			
			if conf > maxClassConf {
				maxClassConf = conf
			}
		}
		
		if maxClassConf > bestConf {
			bestConf = maxClassConf
			bestIdx = i
		}
	}
	
	if bestConf > 0.4 && bestIdx != -1 { // Threshold 0.4
		// Decode box
		cx := outputData[0*anchors + bestIdx]
		cy := outputData[1*anchors + bestIdx]
		w  := outputData[2*anchors + bestIdx]
		h  := outputData[3*anchors + bestIdx]
		
		// Coordinates are relative to 640x640
		// Convert to original image coordinates
		
		origW := float32(img.Width())
		origH := float32(img.Height())
		
		scaleX := origW / 640.0
		scaleY := origH / 640.0
		
		// Box center and size in original image
		boxX := (cx - w/2) * scaleX
		boxY := (cy - h/2) * scaleY
		boxW := w * scaleX
		boxH := h * scaleY
		
		rect := image.Rect(
			int(boxX), int(boxY),
			int(boxX + boxW), int(boxY + boxH),
		)
		
		// Clamp
		rect = rect.Intersect(image.Rect(0, 0, int(origW), int(origH)))
		
		slog.Info("AI Smart Crop found object", "conf", bestConf, "rect", rect)
		return &rect, nil
	}
	
	runtime.KeepAlive(outputData)
	return nil, nil
}

// SmartCrop applies the smart crop logic.
func SmartCrop(img *vips.ImageRef, width, height int, detector ObjectDetector) error {
	// If detector returns a specific rect, we crop to it.
	// If not (nil), we use vips built-in Entropy.
	
	if detector != nil {
		rect, err := detector.Detect(img)
		if err == nil && rect != nil {
			// Crop to rect
			if err := img.ExtractArea(rect.Min.X, rect.Min.Y, rect.Dx(), rect.Dy()); err != nil {
				return err
			}
			// Then resize to target if needed
			return img.ThumbnailWithSize(width, height, vips.InterestingCentre, vips.SizeForce)
		}
	}

	// Default Vips Entropy
	return img.ThumbnailWithSize(width, height, vips.InterestingEntropy, vips.SizeForce)
}
