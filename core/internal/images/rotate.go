package images

import (
	"bytes"
	"image"
	"image/jpeg"
	"log"
)

// portraitForPrint: a landscape JPEG turned a quarter clockwise, so it fills an A4 page held
// sideways whatever the print service does with the page; portrait ones and undecodable data as
// they are. 1536x1024 takes ~6 MB while turning (the Echo has room).
func portraitForPrint(data []byte) []byte {
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	b := src.Bounds()
	if b.Dx() <= b.Dy() {
		return data
	}
	dst := image.NewRGBA(image.Rect(0, 0, b.Dy(), b.Dx()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			// clockwise: the left column becomes the top row
			dst.Set(b.Max.Y-1-y, x-b.Min.X, src.At(x, y))
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 90}); err != nil {
		log.Printf("[Image] turning for print: %v", err)
		return data
	}
	return out.Bytes()
}
