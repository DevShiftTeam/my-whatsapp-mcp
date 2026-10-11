package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// Removing the predecode dimension check must reject this valid PNG header
// before a decoder can allocate a 400-million-pixel image.
func TestImagePixelBombBeforeDecode(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	binary.BigEndian.PutUint32(data[16:20], 20000)
	binary.BigEndian.PutUint32(data[20:24], 20000)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if _, err := inspectImage(bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrLimit) {
		t.Fatalf("pixel bomb accepted: %v", err)
	}
	if _, err := inspectImage(bytes.NewReader([]byte("not an image")), 12); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt image: %v", err)
	}
}

func TestImageOrientationAndCaptionPixelBound(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.Set(0, 0, color.NRGBA{R: 255, A: 255})
	src.Set(1, 0, color.NRGBA{B: 255, A: 255})
	rotated, err := orientImage(context.Background(), src, 6)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Bounds().Dx() != 1 || rotated.Bounds().Dy() != 2 || color.NRGBAModel.Convert(rotated.At(0, 0)).(color.NRGBA).R != 255 {
		t.Fatal("EXIF clockwise orientation lost")
	}
	w, h := captionDimensions(4000, 3000)
	if w < 1 || h < 1 || int64(w)*int64(h) > 262144 || w*3-h*4 > 4 || h*4-w*3 > 4 {
		t.Fatalf("caption size %dx%d", w, h)
	}
	w, h = captionDimensions(1, 16000000)
	if int64(w)*int64(h) > 262144 {
		t.Fatal("extreme aspect ratio escaped caption pixel cap")
	}
}

func TestImagePreprocessingHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	if _, err := orientImage(ctx, src, 6); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := resizeCaption(ctx, src); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reader := imageContextReader{ctx, bytes.NewReader([]byte("bytes"))}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestEXIFOrientationRequiresCheckedTIFFOffsets(t *testing.T) {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 6, 0, 0, 0}
	if got := tiffOrientation(tiff); got != 6 {
		t.Fatalf("valid orientation %d", got)
	}
	tiff[4] = 255
	tiff[5] = 255
	tiff[6] = 255
	tiff[7] = 255
	if got := tiffOrientation(tiff); got != 1 {
		t.Fatal("unbounded EXIF offset trusted")
	}
}

// Virtual source pixels keep extreme-aspect coverage bounded without allocating
// the original 16-million-pixel image. The four corners have distinct colors.
type captionCornerImage struct{ bounds image.Rectangle }

func (s captionCornerImage) ColorModel() color.Model { return color.NRGBAModel }
func (s captionCornerImage) Bounds() image.Rectangle { return s.bounds }
func (s captionCornerImage) At(x, y int) color.Color {
	var red, green uint8
	if x == s.bounds.Max.X-1 {
		red = 255
	}
	if y == s.bounds.Max.Y-1 {
		green = 255
	}
	return color.NRGBA{R: red, G: green, B: 42, A: 255}
}

func TestCaptionCanvasPreservesWholeImageAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		width, height, fitWidth, fitHeight int
	}{
		{"square", 512, 512, 512, 512},
		{"landscape", 4, 2, 512, 256},
		{"portrait", 2, 4, 256, 512},
		{"tiny", 1, 1, 512, 512},
		{"extreme horizontal", 16000000, 1, 512, 1},
		{"extreme vertical", 1, 16000000, 1, 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := captionCornerImage{image.Rect(11, 23, 11+tc.width, 23+tc.height)}
			canvas, err := resizeCaption(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			if canvas.Bounds() != image.Rect(0, 0, 512, 512) {
				t.Fatal("canvas axes are not fixed")
			}
			x0, y0 := (512-tc.fitWidth)/2, (512-tc.fitHeight)/2
			content := image.Rect(x0, y0, x0+tc.fitWidth, y0+tc.fitHeight)
			for y := 0; y < 512; y++ {
				for x := 0; x < 512; x++ {
					pixel := color.NRGBAModel.Convert(canvas.At(x, y)).(color.NRGBA)
					if pixel.A != 255 {
						t.Fatal("canvas is not opaque RGB")
					}
					if !image.Pt(x, y).In(content) && pixel != (color.NRGBA{127, 127, 127, 255}) {
						t.Fatal("padding not centered neutral RGB")
					}
				}
			}
			for _, pair := range [][2]image.Point{
				{{content.Min.X, content.Min.Y}, {src.bounds.Min.X, src.bounds.Min.Y}},
				{{content.Max.X - 1, content.Min.Y}, {src.bounds.Max.X - 1, src.bounds.Min.Y}},
				{{content.Min.X, content.Max.Y - 1}, {src.bounds.Min.X, src.bounds.Max.Y - 1}},
				{{content.Max.X - 1, content.Max.Y - 1}, {src.bounds.Max.X - 1, src.bounds.Max.Y - 1}},
			} {
				if canvas.At(pair[0].X, pair[0].Y) != src.At(pair[1].X, pair[1].Y) {
					t.Fatal("source edge was cropped or reordered")
				}
			}
		})
	}
	transparent := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	transparent.SetNRGBA(0, 0, color.NRGBA{R: 255})
	canvas, err := resizeCaption(context.Background(), transparent)
	if err != nil || canvas.At(0, 0) != (color.NRGBA{127, 127, 127, 255}) {
		t.Fatal("transparent input did not composite onto neutral canvas")
	}
	if _, err := resizeCaption(context.Background(), image.NewNRGBA(image.Rectangle{})); !errors.Is(err, ErrLimit) {
		t.Fatal("empty input accepted")
	}
}

type captionCancelImage struct {
	image.Image
	cancel context.CancelFunc
	reads  int
}

func (s *captionCancelImage) At(x, y int) color.Color {
	s.reads++
	if s.reads == 1 {
		s.cancel()
	}
	return s.Image.At(x, y)
}
func TestCaptionCanvasCancelsDuringResize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &captionCancelImage{Image: captionCornerImage{image.Rect(0, 0, 512, 512)}, cancel: cancel}
	result, err := resizeCaption(ctx, src)
	if !errors.Is(err, context.Canceled) || result != nil || src.reads == 0 || src.reads > 512 {
		t.Fatalf("in-progress cancellation lost: reads=%d error=%v", src.reads, err)
	}
}
