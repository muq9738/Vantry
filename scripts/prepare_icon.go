//go:build ignore

// Prepare the supplied artwork for the PNG and macOS app icon resources.
// Run from the repository root: go run scripts/prepare_icon.go
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

func main() {
	source := flag.String("source", "assets/branding/icon-original.png", "original square PNG")
	output := flag.String("out", "resources/icon.png", "1024px application icon")
	flag.Parse()
	if err := prepare(*source, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func prepare(source, output string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	original, err := png.Decode(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	bounds := original.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width != height {
		return fmt.Errorf("icon artwork must be square: %dx%d", width, height)
	}
	icon := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Copy(icon, image.Point{}, original, bounds, draw.Src, nil)

	// Only remove near-black pixels connected to the outside of the image.
	// The illustration's black hair and outlines are enclosed by its white tile.
	outside := make([]bool, width*height)
	queue := make([]int, 0, width*height/4)
	visit := func(x, y int) {
		index := y*width + x
		if outside[index] {
			return
		}
		pixel := icon.NRGBAAt(x, y)
		if pixel.R <= 32 && pixel.G <= 32 && pixel.B <= 32 {
			outside[index] = true
			queue = append(queue, index)
		}
	}
	for x := 0; x < width; x++ {
		visit(x, 0)
		visit(x, height-1)
	}
	for y := 0; y < height; y++ {
		visit(0, y)
		visit(width-1, y)
	}
	for head := 0; head < len(queue); head++ {
		index := queue[head]
		x, y := index%width, index/width
		if x > 0 {
			visit(x-1, y)
		}
		if x+1 < width {
			visit(x+1, y)
		}
		if y > 0 {
			visit(x, y-1)
		}
		if y+1 < height {
			visit(x, y+1)
		}
	}
	for _, index := range queue {
		icon.SetNRGBA(index%width, index/width, color.NRGBA{})
	}
	resized := image.NewNRGBA(image.Rect(0, 0, 1024, 1024))
	draw.CatmullRom.Scale(resized, resized.Bounds(), icon, icon.Bounds(), draw.Src, nil)
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	target, err := os.Create(output)
	if err != nil {
		return err
	}
	if err := png.Encode(target, resized); err != nil {
		_ = target.Close()
		return err
	}
	if err := target.Close(); err != nil {
		return err
	}
	fmt.Printf("%s: %dx%d → 1024x1024; %d edge background pixels made transparent\n", output, width, height, len(queue))
	return nil
}
