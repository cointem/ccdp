package protocol

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"testing"
)

func testInputPNG(t *testing.T) InputImage {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return InputImage{MediaType: "image/png", Data: b.Bytes()}
}

func TestValidateInputImages(t *testing.T) {
	img := testInputPNG(t)
	if err := ValidateInputImages([]InputImage{img}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := InspectInputImage(img)
	if cfg.Width != 2 || cfg.Height != 3 {
		t.Fatal(cfg)
	}
	for name, images := range map[string][]InputImage{
		"empty":         {{MediaType: "image/png"}},
		"invalid":       {{MediaType: "image/png", Data: []byte("not an image")}},
		"mime mismatch": {{MediaType: "image/jpeg", Data: img.Data}},
		"oversize":      {{MediaType: "image/png", Data: make([]byte, MaxInputImageBytes+1)}},
		"count":         make([]InputImage, MaxInputImages+1),
	} {
		t.Run(name, func(t *testing.T) {
			if ValidateInputImages(images) == nil {
				t.Fatal("accepted invalid images")
			}
		})
	}
	copy := CloneInputImages([]InputImage{img})
	copy[0].Data[0] = 0
	if img.Data[0] == 0 {
		t.Fatal("clone aliases caller bytes")
	}
}

func TestInputImageDimensionsAndAggregateBudget(t *testing.T) {
	img := testInputPNG(t)
	for _, size := range [][2]uint32{{16385, 1}, {8000, 8000}} {
		data := bytes.Clone(img.Data)
		binary.BigEndian.PutUint32(data[16:20], size[0])
		binary.BigEndian.PutUint32(data[20:24], size[1])
		binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
		if _, err := InspectInputImage(InputImage{MediaType: "image/png", Data: data}); err == nil {
			t.Fatal("oversized dimensions accepted")
		}
	}
	data := make([]byte, MaxInputImageBytes)
	copy(data, img.Data)
	large := InputImage{MediaType: "image/png", Data: data}
	if err := ValidateInputImages([]InputImage{large, large}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateInputImages([]InputImage{large, large, large}); err == nil {
		t.Fatal("aggregate budget bypassed")
	}
}
