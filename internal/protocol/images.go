package protocol

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

const (
	MaxInputImages      = 10
	MaxInputImageBytes  = 20 << 20
	MaxInputImagesBytes = 40 << 20
	MaxInputImagePixels = 40_000_000
)

// InputImage is a client-captured image, not a path the runtime may open.
// Data is base64 on the JSON wire and becomes a scoped blob at admission.
type InputImage struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

// InspectInputImage checks encoded size, format and dimensions without
// allocating a decoded pixel buffer. Clipboard adapters normalize to these
// portable formats; legacy Markdown file attachments retain their own rules.
func InspectInputImage(img InputImage) (image.Config, error) {
	if len(img.Data) == 0 || len(img.Data) > MaxInputImageBytes {
		return image.Config{}, fmt.Errorf("image must contain 1–%d bytes", MaxInputImageBytes)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(img.Data))
	if err != nil {
		return image.Config{}, fmt.Errorf("invalid clipboard image: %w", err)
	}
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}[format]
	if mime == "" || img.MediaType != mime {
		return image.Config{}, fmt.Errorf("image media type %q does not match %s", img.MediaType, format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 16384 || cfg.Height > 16384 || int64(cfg.Width)*int64(cfg.Height) > MaxInputImagePixels {
		return image.Config{}, fmt.Errorf("image dimensions exceed 16384 per side or %d pixels", MaxInputImagePixels)
	}
	return cfg, nil
}

func ValidateInputImages(images []InputImage) error {
	if len(images) > MaxInputImages {
		return fmt.Errorf("at most %d images per input", MaxInputImages)
	}
	total := 0
	for i, img := range images {
		total += len(img.Data)
		if total > MaxInputImagesBytes {
			return fmt.Errorf("images exceed %d bytes total", MaxInputImagesBytes)
		}
		if _, err := InspectInputImage(img); err != nil {
			return fmt.Errorf("image %d: %w", i+1, err)
		}
	}
	return nil
}

func CloneInputImages(images []InputImage) []InputImage {
	if images == nil {
		return nil
	}
	out := make([]InputImage, len(images))
	for i, img := range images {
		out[i] = InputImage{MediaType: img.MediaType, Data: bytes.Clone(img.Data)}
	}
	return out
}
