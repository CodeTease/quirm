package processor

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buckket/go-blurhash"
	"github.com/davidbyttow/govips/v2/vips"
	pigo "github.com/esimov/pigo/core"

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
	Blurhash         bool
	SmartCompression bool
}

// Process decodes, transforms, watermarks, and encodes the image.
func Process(r io.Reader, opts ImageOptions, wmImg image.Image, wmOpacity float64, originalKey string) (*bytes.Buffer, error) {
	start := time.Now()
	defer func() {
		metrics.ImageProcessDuration.Observe(time.Since(start).Seconds())
	}()

	// 1. Decode
	img, err := vips.NewImageFromReader(r)
	if err != nil {
		metrics.ImageProcessErrorsTotal.Inc()
		return nil, fmt.Errorf("decode error: %w", err)
	}
	defer img.Close()

	// 2. Transform
	if opts.Width > 0 || opts.Height > 0 {
		switch opts.Fit {
		case "cover":
			if opts.Focus == "smart" {
				if err := img.ThumbnailWithSize(opts.Width, opts.Height, vips.InterestingEntropy, vips.SizeForce); err != nil {
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

		svg := fmt.Sprintf(`<svg width="%d" height="%d">
            <text x="50%%" y="50%%" font-family="sans-serif" font-size="%f" fill="%s" text-anchor="middle" dominant-baseline="middle" opacity="%f">%s</text>
        </svg>`, img.Width(), img.Height(), opts.TextSize, opts.TextColor, 1.0, opts.Text)

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

	switch format {
	case "png":
		ep := vips.NewPngExportParams()
		ep.Quality = quality
		ep.StripMetadata = true
		if smart {
			ep.Compression = 9 // Max compression
		}
		return img.ExportPng(ep)
	case "webp":
		ep := vips.NewWebpExportParams()
		ep.Quality = quality
		ep.StripMetadata = true
		if smart {
			ep.ReductionEffort = 6
		}
		return img.ExportWebp(ep)
	case "avif":
		ep := vips.NewAvifExportParams()
		ep.Quality = quality
		ep.StripMetadata = true
		if smart {
			ep.Speed = 0 // Slowest but best size
		}
		return img.ExportAvif(ep)
	case "gif":
		ep := vips.NewGifExportParams()
		ep.Quality = quality
		ep.StripMetadata = true
		return img.ExportGIF(ep)
	case "jxl":
		// JXL Support via Generic Export or custom
		// govips v2.16 might not have NewJxlExportParams yet.
		// Using generic ExportParams.
		ep := vips.NewDefaultExportParams()
		ep.Format = vips.ImageTypeJXL
		ep.Quality = quality
		ep.StripMetadata = true
		if smart {
			ep.Effort = 7 // Higher effort
		}
		return img.Export(ep)
	case "jpeg", "jpg":
		ep := vips.NewJpegExportParams()
		ep.Quality = quality
		ep.StripMetadata = true
		if smart {
			ep.Interlace = true
			ep.OptimizeCoding = true
			ep.TrellisQuant = true
		}
		return img.ExportJpeg(ep)
	default:
		ep := vips.NewJpegExportParams()
		ep.Quality = quality
		return img.ExportJpeg(ep)
	}
}
