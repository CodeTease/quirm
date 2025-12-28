package processor

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/buckket/go-blurhash"
	"github.com/davidbyttow/govips/v2/vips"
	pigo "github.com/esimov/pigo/core"
	"go.opentelemetry.io/otel"

	"github.com/CodeTease/quirm/pkg/metrics"
)

var cascadeParams []byte

// LoadCascade loads the pigo cascade file from the given path.
func LoadCascade(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cascadeParams = b
	return nil
}

type ImageOptions struct {
	Width            int
	Height           int
	Fit              string // cover, contain, fill, inside
	Format           string // jpeg, png, webp, jxl
	Quality          int
	Focus            string // smart, face
	Text             string
	TextColor        string
	TextSize         float64
	TextOpacity      float64
	Font             string
	Effect           string
	Brightness       float64
	Contrast         float64
	Blurhash         bool
	SmartCompression bool
	Animated         bool
	Page             int
}

// Process decodes, transforms, watermarks, and encodes the image.
// Note: We cannot easily pass context here without changing the signature, but typically Process is called from HandleRequest
// which has a context. Ideally Process should take context. For now we use Background if we can't change signature,
// BUT looking at where Process is called, it might be inside HandleRequest which likely has context.
func Process(ctx context.Context, r io.Reader, opts ImageOptions, wmImg image.Image, wmOpacity float64, originalKey string) (*bytes.Buffer, error) {
	tracer := otel.Tracer("quirm/processor")
	ctx, span := tracer.Start(ctx, "Processor.Process")
	defer span.End()

	start := time.Now()
	defer func() {
		metrics.ImageProcessDuration.Observe(time.Since(start).Seconds())
	}()

	// 1. Decode
	// We read the full stream into memory to support LoadImageFromBuffer with options (e.g. Page)
	data, err := io.ReadAll(r)
	if err != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, fmt.Errorf("read error: %w", err)
	}

	importParams := vips.NewImportParams()
	if opts.Page > 0 {
		importParams.Page.Set(opts.Page - 1)
	}

	img, err := vips.LoadImageFromBuffer(data, importParams)
	if err != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, fmt.Errorf("decode error: %w", err)
	}
	defer img.Close()

	// PDF Specific Logic
	// If the image is a PDF, we might need to handle transparency (flatten to white)
	// because PDFs are often transparent and saving as JPEG results in black background.
	// Also ensures that if it's a multi-page PDF, we are working with the loaded page (default first page).
	if img.OriginalFormat() == vips.ImageTypePDF {
		// Flatten to white if it has alpha
		if img.HasAlpha() {
			// Flatten with white background
			// libvips flatten uses the background color parameter
			// govips Flatten uses a Color struct
			white := &vips.Color{R: 255, G: 255, B: 255}
			if err := img.Flatten(white); err != nil {
				// Log but continue
				fmt.Printf("Error flattening PDF: %v\n", err)
			}
		}
	}

	// 2. Transform
	if opts.Width > 0 || opts.Height > 0 {
		switch opts.Fit {
		case "cover":
			if opts.Focus == "smart" {
				// Use AI Detector if configured/available, else fallback to Entropy
				// For now we instantiate a detector. In a real app, this should be a singleton injected.
				detector := &AiDetector{}
				if err := SmartCrop(img, opts.Width, opts.Height, detector); err != nil {
					return nil, err
				}
			} else if opts.Focus == "face" {
				if len(cascadeParams) > 0 {
					detImg, err := img.Copy()
					if err != nil {
						return nil, err
					}

					if err := detImg.ToColorSpace(vips.InterpretationBW); err != nil {
						detImg.Close()
						return nil, err
					}
					pixels, err := detImg.ToBytes()
					if err != nil {
						detImg.Close()
						return nil, err
					}
					cols := detImg.Width()
					rows := detImg.Height()
					detImg.Close()

					cParams := pigo.NewPigo()
					classifier, err := cParams.Unpack(cascadeParams)
					if err == nil {
						imgParams := pigo.ImageParams{
							Pixels: pixels,
							Rows:   rows,
							Cols:   cols,
							Dim:    cols,
						}
						cascade := pigo.CascadeParams{
							MinSize:     20,
							MaxSize:     1000,
							ShiftFactor: 0.1,
							ScaleFactor: 1.1,
							ImageParams: imgParams,
						}

						dets := classifier.RunCascade(cascade, 0.0)
						dets = classifier.ClusterDetections(dets, 0.2)

						if len(dets) > 0 {
							var maxDet pigo.Detection
							maxSize := 0
							for _, det := range dets {
								if det.Scale > maxSize {
									maxSize = det.Scale
									maxDet = det
								}
							}

							faceX := maxDet.Col
							faceY := maxDet.Row

							targetRatio := float64(opts.Width) / float64(opts.Height)
							srcRatio := float64(cols) / float64(rows)

							var cropW, cropH int
							if srcRatio > targetRatio {
								cropH = rows
								cropW = int(float64(cropH) * targetRatio)
							} else {
								cropW = cols
								cropH = int(float64(cropW) / targetRatio)
							}

							x0 := faceX - cropW/2
							y0 := faceY - cropH/2

							if x0 < 0 {
								x0 = 0
							}
							if y0 < 0 {
								y0 = 0
							}
							if x0+cropW > cols {
								x0 = cols - cropW
							}
							if y0+cropH > rows {
								y0 = rows - cropH
							}
							if err := img.ExtractArea(x0, y0, cropW, cropH); err != nil {
								return nil, err
							}
							if err := img.Resize(float64(opts.Width)/float64(cropW), vips.KernelLanczos3); err != nil {
								return nil, err
							}

						} else {
							if err := img.ThumbnailWithSize(opts.Width, opts.Height, vips.InterestingCentre, vips.SizeForce); err != nil {
								return nil, err
							}
						}
					} else {
						if err := img.ThumbnailWithSize(opts.Width, opts.Height, vips.InterestingCentre, vips.SizeForce); err != nil {
							return nil, err
						}
					}

				} else {
					if err := img.ThumbnailWithSize(opts.Width, opts.Height, vips.InterestingCentre, vips.SizeForce); err != nil {
						return nil, err
					}
				}
			} else {
				if err := img.ThumbnailWithSize(opts.Width, opts.Height, vips.InterestingCentre, vips.SizeForce); err != nil {
					return nil, err
				}
			}
		case "contain":
			scale := float64(opts.Width) / float64(img.Width())
			scaleY := float64(opts.Height) / float64(img.Height())
			if scaleY < scale {
				scale = scaleY
			}
			if err := img.Resize(scale, vips.KernelLanczos3); err != nil {
				return nil, err
			}

		default:
			if err := img.ResizeWithVScale(float64(opts.Width)/float64(img.Width()), float64(opts.Height)/float64(img.Height()), vips.KernelLanczos3); err != nil {
				return nil, err
			}
		}
	}

	// 2.5 Effects
	if err := applyEffects(img, opts); err != nil {
		return nil, err
	}

	// 3. Watermark (Image)
	if wmImg != nil {
		var wmBuf bytes.Buffer
		if err := png.Encode(&wmBuf, wmImg); err == nil {
			wmVips, err := vips.NewImageFromBuffer(wmBuf.Bytes())
			if err == nil {
				x := img.Width() - wmVips.Width() - 10
				y := img.Height() - wmVips.Height() - 10
				if x < 0 {
					x = 0
				}
				if y < 0 {
					y = 0
				}

				if wmOpacity < 1.0 {
					if err := wmVips.Linear([]float64{1, 1, 1, wmOpacity}, []float64{0, 0, 0, 0}); err != nil {
						// ignore
					}
				}

				if err := img.Composite(wmVips, vips.BlendModeOver, x, y); err != nil {
					// ignore
				}
				wmVips.Close()
			}
		}
	}

	// 3.5 Text Overlay
	if opts.Text != "" {
		if opts.TextSize == 0 {
			opts.TextSize = 24
		}
		if opts.TextColor == "" {
			opts.TextColor = "red"
		}
		fontFamily := opts.Font
		if fontFamily == "" {
			fontFamily = "sans-serif"
		} else {
			// Sanitize Font Name: Allow only alphanumeric, space, hyphens, and underscores
			// to prevent SVG injection.
			safe := true
			for _, r := range fontFamily {
				if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == ' ' || r == '-' || r == '_') {
					safe = false
					break
				}
			}
			if !safe {
				fontFamily = "sans-serif"
			}
		}

		textOpacity := opts.TextOpacity
		if textOpacity == 0 {
			textOpacity = 1.0
		}

		svg := fmt.Sprintf(`<svg width="%d" height="%d">
			<text x="50%%" y="50%%" font-family="%s" font-size="%f" fill="%s" text-anchor="middle" dominant-baseline="middle" opacity="%f">%s</text>
		</svg>`, img.Width(), img.Height(), fontFamily, opts.TextSize, opts.TextColor, textOpacity, opts.Text)

		textImg, err := vips.NewImageFromBuffer([]byte(svg))
		if err == nil {
			if err := img.Composite(textImg, vips.BlendModeOver, 0, 0); err != nil {
				fmt.Println("Text composite error:", err)
			}
			textImg.Close()
		}
	}

	// 4. Encode
	// Handle Blurhash
	if opts.Blurhash {
		thumb, err := img.Copy()
		if err != nil {
			return nil, err
		}
		if err := thumb.ThumbnailWithSize(32, 32, vips.InterestingCentre, vips.SizeForce); err != nil {
			thumb.Close()
			return nil, err
		}

		if err := thumb.ToColorSpace(vips.InterpretationSRGB); err != nil {
			thumb.Close()
			return nil, err
		}

		pixels, err := thumb.ToBytes()
		if err != nil {
			thumb.Close()
			return nil, err
		}
		w := thumb.Width()
		h := thumb.Height()
		bands := thumb.Bands()
		thumb.Close()

		var imgObj image.Image
		if bands == 4 {
			imgObj = &image.RGBA{
				Pix:    pixels,
				Stride: w * 4,
				Rect:   image.Rect(0, 0, w, h),
			}
		} else if bands == 3 {
			rgbaPixels := make([]uint8, w*h*4)
			for i := 0; i < w*h; i++ {
				rgbaPixels[i*4] = pixels[i*3]
				rgbaPixels[i*4+1] = pixels[i*3+1]
				rgbaPixels[i*4+2] = pixels[i*3+2]
				rgbaPixels[i*4+3] = 255
			}
			imgObj = &image.RGBA{Pix: rgbaPixels, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
		} else {
			return nil, fmt.Errorf("unsupported bands for blurhash: %d", bands)
		}

		hash, err := blurhash.Encode(4, 3, imgObj)
		if err != nil {
			metrics.ImageProcessErrorsTotal.Inc()
			return nil, err
		}
		return bytes.NewBufferString(hash), nil
	}

	// Actual Encode
	formatStr := strings.ToLower(opts.Format)
	if formatStr == "" {
		ext := strings.ToLower(filepath.Ext(originalKey))
		if ext == ".png" {
			formatStr = "png"
		} else if ext == ".gif" {
			formatStr = "gif"
		} else if ext == ".webp" {
			formatStr = "webp"
		} else if ext == ".avif" {
			formatStr = "avif"
		} else if ext == ".jxl" {
			formatStr = "jxl"
		} else {
			formatStr = "jpeg"
		}
	}

	exportBytes, _, err := exportImage(img, formatStr, opts.Quality, opts.SmartCompression)
	if err != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, err
	}

	return bytes.NewBuffer(exportBytes), nil
}

func exportImage(img *vips.ImageRef, format string, quality int, smart bool) ([]byte, *vips.ImageMetadata, error) {
	if quality == 0 {
		quality = 80
	}

	// Unconditionally force strip metadata
	stripMetadata := true

	switch format {
	case "png":
		ep := vips.NewPngExportParams()
		ep.Quality = quality
		ep.StripMetadata = stripMetadata
		if smart {
			ep.Compression = 9 // Max compression
			ep.Palette = true  // Use palette if possible
		}
		return img.ExportPng(ep)
	case "webp":
		ep := vips.NewWebpExportParams()
		ep.Quality = quality
		ep.StripMetadata = stripMetadata
		if smart {
			ep.ReductionEffort = 6
		}
		return img.ExportWebp(ep)
	case "avif":
		ep := vips.NewAvifExportParams()
		ep.Quality = quality
		ep.StripMetadata = stripMetadata
		if smart {
			ep.Speed = 0 // Slowest but best size
		}
		return img.ExportAvif(ep)
	case "gif":
		ep := vips.NewGifExportParams()
		ep.Quality = quality
		ep.StripMetadata = stripMetadata
		return img.ExportGIF(ep)
	case "jxl":
		ep := vips.NewJxlExportParams()
		ep.Quality = quality
		// For JXL, govips sets lossless if quality is 100 or default
		if quality == 100 {
			ep.Lossless = true
		}
		if smart {
			ep.Effort = 7 // Higher effort
		}
		return img.ExportJxl(ep)
	case "jpeg", "jpg":
		ep := vips.NewJpegExportParams()
		ep.Quality = quality
		ep.StripMetadata = stripMetadata
		if smart {
			ep.Interlace = true
			ep.OptimizeCoding = true
			ep.TrellisQuant = true
		}
		return img.ExportJpeg(ep)
	default:
		ep := vips.NewJpegExportParams()
		ep.Quality = quality
		ep.StripMetadata = stripMetadata
		return img.ExportJpeg(ep)
	}
}

// ExtractPalette extracts dominant colors from the image.
func ExtractPalette(r io.Reader) ([]string, error) {
	img, err := vips.NewImageFromReader(r)
	if err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}
	defer img.Close()

	// Resize to small size (100x100) to find dominant colors faster and group them
	if err := img.ThumbnailWithSize(100, 100, vips.InterestingCentre, vips.SizeForce); err != nil {
		return nil, err
	}

	// Ensure sRGB
	if err := img.ToColorSpace(vips.InterpretationSRGB); err != nil {
		return nil, err
	}

	pixels, err := img.ToBytes()
	if err != nil {
		return nil, err
	}

	bands := img.Bands()
	w := img.Width()
	h := img.Height()

	colorCounts := make(map[string]int)

	toHex := func(r, g, b uint8) string {
		return fmt.Sprintf("#%02x%02x%02x", r, g, b)
	}

	for i := 0; i < w*h; i++ {
		offset := i * bands
		if offset+bands > len(pixels) {
			break
		}

		var rVal, gVal, bVal uint8

		if bands >= 3 {
			rVal = pixels[offset]
			gVal = pixels[offset+1]
			bVal = pixels[offset+2]
		} else if bands == 1 {
			// Grayscale
			val := pixels[offset]
			rVal, gVal, bVal = val, val, val
		} else if bands == 2 {
			// Grayscale + Alpha?
			val := pixels[offset]
			rVal, gVal, bVal = val, val, val
		} else {
			// Fallback (shouldn't happen with sRGB/BW)
			continue
		}

		hex := toHex(rVal, gVal, bVal)
		colorCounts[hex]++
	}

	type colorFreq struct {
		Hex   string
		Count int
	}
	var freqs []colorFreq
	for k, v := range colorCounts {
		freqs = append(freqs, colorFreq{k, v})
	}

	sort.Slice(freqs, func(i, j int) bool {
		return freqs[i].Count > freqs[j].Count
	})

	limit := 5
	if len(freqs) < limit {
		limit = len(freqs)
	}

	result := make([]string, limit)
	for i := 0; i < limit; i++ {
		result[i] = freqs[i].Hex
	}

	return result, nil
}

func applyEffects(img *vips.ImageRef, opts ImageOptions) error {
	hasAlpha := img.HasAlpha()

	// Effect: Grayscale
	if opts.Effect == "grayscale" {
		if err := img.ToColorSpace(vips.InterpretationBW); err != nil {
			return err
		}
		// If it had alpha, ToColorSpace(BW) might drop it or handle it depending on implementation.
		// vips usually handles it. If not, we might lose alpha.
		// But for grayscale, usually fine.
	}

	// Effect: Sepia
	if opts.Effect == "sepia" {
		// Standard Sepia Matrix
		// R = tr*0.393 + tg*0.769 + tb*0.189 (Use slightly different standard values in code below)
		// 0.3588, 0.7044, 0.1368

		var matrix [][]float64
		if hasAlpha {
			// 4x4 Identity for Alpha
			matrix = [][]float64{
				{0.3588, 0.7044, 0.1368, 0},
				{0.2990, 0.5870, 0.1140, 0},
				{0.2392, 0.4696, 0.0912, 0},
				{0, 0, 0, 1},
			}
		} else {
			matrix = [][]float64{
				{0.3588, 0.7044, 0.1368},
				{0.2990, 0.5870, 0.1140},
				{0.2392, 0.4696, 0.0912},
			}
		}

		if err := img.Recomb(matrix); err != nil {
			return err
		}
	}

	// Brightness
	if opts.Brightness != 0 {
		// Linear: output = input * a + b
		// Brightness is additive, so a=1, b=brightness

		var a, b []float64
		if hasAlpha {
			a = []float64{1, 1, 1, 1}
			b = []float64{opts.Brightness, opts.Brightness, opts.Brightness, 0}
		} else {
			a = []float64{1, 1, 1}
			b = []float64{opts.Brightness, opts.Brightness, opts.Brightness}
		}

		if err := img.Linear(a, b); err != nil {
			return err
		}
	}

	// Contrast
	if opts.Contrast != 0 {
		// Contrast is multiplicative around a pivot (usually 128)
		// Formula: new = (old - 128) * contrast + 128

		c := opts.Contrast
		// If c is exactly 1, do nothing
		if c != 1.0 {
			offset := 128.0 * (1.0 - c)

			var a, b []float64
			if hasAlpha {
				a = []float64{c, c, c, 1}
				b = []float64{offset, offset, offset, 0}
			} else {
				a = []float64{c, c, c}
				b = []float64{offset, offset, offset}
			}

			if err := img.Linear(a, b); err != nil {
				return err
			}
		}
	}

	return nil
}
