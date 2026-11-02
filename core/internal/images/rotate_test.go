package images

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func testJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w/4; x++ { // the left quarter white, the rest black
		for y := 0; y < h; y++ {
			img.Set(x, y, color.White)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func TestPortraitForPrint(t *testing.T) {
	out, err := jpeg.Decode(bytes.NewReader(portraitForPrint(testJPEG(96, 64))))
	if err != nil {
		t.Fatal(err)
	}
	if b := out.Bounds(); b.Dx() != 64 || b.Dy() != 96 {
		t.Fatalf("turned to %v", b)
	}
	// clockwise: the white left side is now the top
	if r, _, _, _ := out.At(32, 5).RGBA(); r < 0xc000 {
		t.Error("top not white")
	}
	if r, _, _, _ := out.At(32, 90).RGBA(); r > 0x4000 {
		t.Error("bottom not black")
	}
	portrait := testJPEG(64, 96)
	if !bytes.Equal(portraitForPrint(portrait), portrait) {
		t.Error("portrait changed")
	}
	if got := portraitForPrint([]byte("jpeg")); string(got) != "jpeg" {
		t.Error("undecodable data changed")
	}
}

func TestSizeFor(t *testing.T) {
	if sizeFor(false) != "1024x1536" || sizeFor(true) != "1536x1024" {
		t.Errorf("sizes %s / %s", sizeFor(false), sizeFor(true))
	}
}
