//go:build ignore

// Convert the design reference into a small, palette-indexed terminal sprite.
// Run from this directory: go run rasterize.go original-pixel.png 40 54
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
)

var palette = []struct {
	key byte
	rgb color.NRGBA
}{
	{'o', color.NRGBA{0, 0, 0, 255}},
	{'b', color.NRGBA{103, 210, 223, 255}},
	{'w', color.NRGBA{255, 255, 255, 255}},
	{'p', color.NRGBA{246, 211, 163, 255}},
	{'d', color.NRGBA{51, 105, 111, 255}},
	{'s', color.NRGBA{85, 85, 85, 255}},
	{'l', color.NRGBA{190, 190, 190, 255}},
	{'t', color.NRGBA{150, 129, 99, 255}},
	{'h', color.NRGBA{167, 228, 235, 255}},
}

func main() {
	if len(os.Args) != 4 {
		panic("usage: go run rasterize.go SOURCE.png WIDTH HEIGHT")
	}
	w, err := strconv.Atoi(os.Args[2])
	if err != nil || w < 4 {
		panic("invalid width")
	}
	h, err := strconv.Atoi(os.Args[3])
	if err != nil || h < 4 || h%2 != 0 {
		panic("height must be positive and even")
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	src, err := png.Decode(f)
	if err != nil {
		panic(err)
	}
	bounds := src.Bounds()
	// Remove only background-colored pixels connected to the image border.
	// Interior black pupils and white eyes/teeth remain intact. The approved
	// source has a black matte; convert it to empty terminal cells, not a box.
	corner := color.NRGBAModel.Convert(src.At(bounds.Min.X, bounds.Min.Y)).(color.NRGBA)
	darkMatte := max(corner.R, corner.G, corner.B) < 64
	background := map[image.Point]bool{}
	queue := []image.Point{}
	add := func(p image.Point) {
		if !p.In(bounds) || background[p] {
			return
		}
		c := color.NRGBAModel.Convert(src.At(p.X, p.Y)).(color.NRGBA)
		lo, hi := min(c.R, c.G, c.B), max(c.R, c.G, c.B)
		if c.A < 128 || hi-lo < 26 && (darkMatte && hi < 64 || !darkMatte && lo > 75) {
			background[p] = true
			queue = append(queue, p)
		}
	}
	for x := bounds.Min.X; x < bounds.Max.X; x++ {
		add(image.Pt(x, bounds.Min.Y))
		add(image.Pt(x, bounds.Max.Y-1))
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		add(image.Pt(bounds.Min.X, y))
		add(image.Pt(bounds.Max.X-1, y))
	}
	for i := 0; i < len(queue); i++ {
		p := queue[i]
		add(p.Add(image.Pt(-1, 0)))
		add(p.Add(image.Pt(1, 0)))
		add(p.Add(image.Pt(0, -1)))
		add(p.Add(image.Pt(0, 1)))
	}
	crop := image.Rectangle{}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if !background[image.Pt(x, y)] {
				crop = crop.Union(image.Rect(x, y, x+1, y+1))
			}
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x == 0 || y == 0 || x == w-1 || y == h-1 {
				fmt.Print(".")
				continue
			}
			// Area sampling preserves thin eye contours and teeth that a single
			// nearest-neighbor sample can miss. Do not redraw individual features.
			var red, green, blue, covered, total int
			for sy := crop.Min.Y + (y-1)*crop.Dy()/(h-2); sy < crop.Min.Y+y*crop.Dy()/(h-2); sy++ {
				for sx := crop.Min.X + (x-1)*crop.Dx()/(w-2); sx < crop.Min.X+x*crop.Dx()/(w-2); sx++ {
					total++
					if background[image.Pt(sx, sy)] {
						continue
					}
					c := color.NRGBAModel.Convert(src.At(sx, sy)).(color.NRGBA)
					red += int(c.R)
					green += int(c.G)
					blue += int(c.B)
					covered++
				}
			}
			if covered == 0 || covered*2 < total {
				fmt.Print(".")
				continue
			}
			c := color.NRGBA{uint8(red / covered), uint8(green / covered), uint8(blue / covered), 255}
			best, distance := byte('.'), int(^uint(0)>>1)
			for _, entry := range palette {
				if (entry.key == 'p' || entry.key == 't') && int(c.R)-int(c.G) < 15 {
					continue
				}
				if max(c.R, c.G, c.B)-min(c.R, c.G, c.B) < 30 && (entry.key == 'p' || entry.key == 't' || entry.key == 'b' || entry.key == 'd') {
					continue
				}
				dr, dg, db := int(c.R)-int(entry.rgb.R), int(c.G)-int(entry.rgb.G), int(c.B)-int(entry.rgb.B)
				d := dr*dr + dg*dg + db*db
				if d < distance {
					best, distance = entry.key, d
				}
			}
			fmt.Printf("%c", best)
		}
		fmt.Println()
	}
}
