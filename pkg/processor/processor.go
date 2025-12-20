package processor

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buckket/go-blurhash"
	"github.com/chai2010/webp"
	"github.com/disintegration/imaging"
	pigo "github.com/esimov/pigo/core"
	"github.com/fogleman/gg"
	"github.com/gen2brain/avif"
	"github.com/golang/freetype/truetype"
	"github.com/muesli/smartcrop"
	"github.com/muesli/smartcrop/nfnt"
	"golang.org/x/image/font/gofont/goregular"

	"github.com/CodeTease/quirm/pkg/metrics"
)

var cascadeParams []byte

// LoadCascade loads the pigo cascade file from the given path.
// This should be called during application startup.
func LoadCascade(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cascadeParams = b
	return nil
}

type ImageOptions struct {
	Width       int
	Height      int
	Fit         string // cover, contain, fill, inside
	Format      string // jpeg, png, webp
	Quality     int
	Focus       string // smart, face
	Text        string
	TextColor   string
	TextSize    float64
	TextOpacity float64
	Blurhash    bool
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
			if opts.Focus == "smart" {
				analyzer := smartcrop.NewAnalyzer(nfnt.NewDefaultResizer())
				topCrop, err := analyzer.FindBestCrop(img, opts.Width, opts.Height)
				if err == nil {
					type SubImager interface {
						SubImage(r image.Rectangle) image.Image
					}
					if simg, ok := img.(SubImager); ok {
						img = simg.SubImage(topCrop)
						img = imaging.Resize(img, opts.Width, opts.Height, imaging.Lanczos)
					} else {
						// Fallback if not subimager
						img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
					}
				} else {
					// Fallback
					img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
				}
			} else if opts.Focus == "face" {
				if len(cascadeParams) > 0 {
					// Convert to grayscale for pigo
					gray := imaging.Grayscale(img)
					cols, rows := gray.Bounds().Max.X, gray.Bounds().Max.Y
					pixels := make([]uint8, cols*rows)
					for y := 0; y < rows; y++ {
						for x := 0; x < cols; x++ {
							// imaging.Grayscale returns *image.NRGBA where R=G=B
							// We can take any channel
							c := gray.NRGBAAt(x, y)
							pixels[y*cols+x] = c.R
						}
					}

					cParams := pigo.NewPigo()
					// Unpack returns *Pigo, error
					classifier, err := cParams.Unpack(cascadeParams)
					if err == nil {
						// ImageParams for pigo
						imgParams := pigo.ImageParams{
							Pixels: pixels,
							Rows:   rows,
							Cols:   cols,
							Dim:    cols,
						}

						// CascadeParams
						cascade := pigo.CascadeParams{
							MinSize:     20,
							MaxSize:     1000,
							ShiftFactor: 0.1,
							ScaleFactor: 1.1,
							ImageParams: imgParams,
						}

						// Run detection
						dets := classifier.RunCascade(cascade, 0.0) // 0.0 angle
						dets = classifier.ClusterDetections(dets, 0.2)

						if len(dets) > 0 {
							// Find the largest face
							var maxDet pigo.Detection
							maxSize := 0
							for _, det := range dets {
								if det.Scale > maxSize {
									maxSize = det.Scale
									maxDet = det
								}
							}

							// Calculate crop area
							// Center of face is maxDet.Col, maxDet.Row
							// Size is maxDet.Scale
							// We want to crop to opts.Width x opts.Height, centered on face

							// NOTE: pigo returns row/col, which is y/x
							faceX := maxDet.Col
							faceY := maxDet.Row

							// Calculate crop rectangle
							// We want aspect ratio of opts.Width / opts.Height
							targetRatio := float64(opts.Width) / float64(opts.Height)
							srcRatio := float64(cols) / float64(rows)

							var cropW, cropH int
							if srcRatio > targetRatio {
								// Source is wider, crop width
								cropH = rows
								cropW = int(float64(cropH) * targetRatio)
							} else {
								// Source is taller, crop height
								cropW = cols
								cropH = int(float64(cropW) / targetRatio)
							}

							// Center crop on face
							x0 := faceX - cropW/2
							y0 := faceY - cropH/2

							// Clamp
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

							img = imaging.Crop(img, image.Rect(x0, y0, x0+cropW, y0+cropH))
							img = imaging.Resize(img, opts.Width, opts.Height, imaging.Lanczos)
						} else {
							img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
						}
					} else {
						img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
					}
				} else {
					img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
				}
				img = imaging.Fill(img, opts.Width, opts.Height, imaging.Center, imaging.Lanczos)
			}
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

	// 3.5. Text Overlay
	if opts.Text != "" {
		dc := gg.NewContextForImage(img)
		if opts.TextSize == 0 {
			opts.TextSize = 24
		}

		// Load font
		font, err := truetype.Parse(goregular.TTF)
		if err == nil {
			face := truetype.NewFace(font, &truetype.Options{Size: opts.TextSize})
			dc.SetFontFace(face)
		}

		// Set Color
		// Default red if not specified (as per prompt example)
		if opts.TextColor == "" {
			opts.TextColor = "red"
		}

		// Map string color to hex or common names?
		// gg.SetHexColor handles #RRGGBB
		// gg.SetColor handles color.Color
		// For simplicity, let's support basic names or assume hex if starts with #
		switch strings.ToLower(opts.TextColor) {
		case "red":
			dc.SetRGB(1, 0, 0)
		case "green":
			dc.SetRGB(0, 1, 0)
		case "blue":
			dc.SetRGB(0, 0, 1)
		case "white":
			dc.SetRGB(1, 1, 1)
		case "black":
			dc.SetRGB(0, 0, 0)
		default:
			if strings.HasPrefix(opts.TextColor, "#") {
				dc.SetHexColor(opts.TextColor)
			} else {
				dc.SetRGB(1, 0, 0) // Default Red
			}
		}

		// Calculate position (Center for now)
		w := float64(dc.Width())
		h := float64(dc.Height())

		dc.DrawStringAnchored(opts.Text, w/2, h/2, 0.5, 0.5)
		img = dc.Image()
	}

	// 4. Encode
	buf := new(bytes.Buffer)

	if opts.Blurhash {
		// Generate Blurhash
		// We need to resize image to small size for blurhash generation speed (e.g. 32x32)
		// But blurhash expects X and Y components. 4x3 is standard.
		// Wait, Encode(img, x, y).
		// We should probably resize the image down first to speed up encoding if it's large.
		smallImg := imaging.Resize(img, 32, 32, imaging.Lanczos)
		hash, err := blurhash.Encode(4, 3, smallImg)
		if err != nil {
			metrics.ImageProcessErrorsTotal.Inc()
			return nil, err
		}
		buf.WriteString(hash)
		return buf, nil
	}

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
