package media

import (
	"context"
	"encoding/binary"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"

	_ "golang.org/x/image/webp"
)

const imageMaximumBytes int64 = 10 << 20
const imageMaximumPixels int64 = 16000000
const captionMaximumPixels int64 = 262144
const captionCanvasSide = 512

type imageInfo struct {
	Width, Height, Orientation int
	Format                     string
}

func inspectImage(r io.ReaderAt, size int64) (imageInfo, error) {
	if size < 1 {
		return imageInfo{}, ErrCorrupt
	}
	if size > imageMaximumBytes {
		return imageInfo{}, ErrLimit
	}
	cfg, format, err := image.DecodeConfig(io.NewSectionReader(r, 0, size))
	if err != nil {
		return imageInfo{}, ErrCorrupt
	}
	if format != "png" && format != "jpeg" && format != "webp" {
		return imageInfo{}, ErrUnsupported
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width) > imageMaximumPixels/int64(cfg.Height) {
		return imageInfo{}, ErrLimit
	}
	// Metadata probing is bounded independently of compressed file size.
	head := make([]byte, min(size, 1<<20))
	_, _ = r.ReadAt(head, 0)
	return imageInfo{cfg.Width, cfg.Height, imageOrientation(head, format), format}, nil
}

// Unsupported/malformed EXIF never controls allocation or file access. Unknown
// orientation is the unchanged image, not an invented rotation.
func imageOrientation(data []byte, format string) int {
	if format == "jpeg" && len(data) >= 2 {
		for p := 2; p+4 <= len(data); {
			if data[p] != 255 {
				return 1
			}
			marker := data[p+1]
			p += 2
			if marker == 0xda || marker == 0xd9 {
				return 1
			}
			if marker == 0xd8 || marker == 0x01 {
				continue
			}
			n := int(binary.BigEndian.Uint16(data[p : p+2]))
			if n < 2 || p+n > len(data) {
				return 1
			}
			if marker == 0xe1 && n >= 8 && string(data[p+2:p+8]) == "Exif\x00\x00" {
				return tiffOrientation(data[p+8 : p+n])
			}
			p += n
		}
	}
	if format == "png" {
		for p := 8; p+12 <= len(data); {
			n := uint64(binary.BigEndian.Uint32(data[p : p+4]))
			if n > uint64(len(data)-p-12) {
				return 1
			}
			end := p + 8 + int(n)
			if string(data[p+4:p+8]) == "eXIf" {
				return tiffOrientation(data[p+8 : end])
			}
			p = end + 4
		}
	}
	if format == "webp" {
		for p := 12; p+8 <= len(data); {
			n := uint64(binary.LittleEndian.Uint32(data[p+4 : p+8]))
			if n > uint64(len(data)-p-8) {
				return 1
			}
			end := p + 8 + int(n)
			if string(data[p:p+4]) == "EXIF" {
				b := data[p+8 : end]
				if len(b) >= 6 && string(b[:6]) == "Exif\x00\x00" {
					b = b[6:]
				}
				return tiffOrientation(b)
			}
			p = end + int(n&1)
		}
	}
	return 1
}
func tiffOrientation(b []byte) int {
	if len(b) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(b[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(b[2:4]) != 42 {
		return 1
	}
	offset := uint64(order.Uint32(b[4:8]))
	if offset > uint64(len(b)-2) {
		return 1
	}
	p := int(offset)
	n := int(order.Uint16(b[p : p+2]))
	p += 2
	if n > (len(b)-p)/12 {
		return 1
	}
	for i := 0; i < n; i++ {
		entry := b[p+i*12 : p+(i+1)*12]
		if order.Uint16(entry[:2]) == 0x112 && order.Uint16(entry[2:4]) == 3 && order.Uint32(entry[4:8]) == 1 {
			v := int(order.Uint16(entry[8:10]))
			if v >= 1 && v <= 8 {
				return v
			}
		}
	}
	return 1
}
func orientImage(ctx context.Context, src image.Image, orientation int) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if orientation <= 1 || orientation > 8 {
		return src, nil
	}
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	ow, oh := w, h
	if orientation >= 5 {
		ow, oh = h, w
	}
	out := image.NewNRGBA(image.Rect(0, 0, ow, oh))
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < w; x++ {
			xx, yy := x, y
			switch orientation {
			case 2:
				xx = w - 1 - x
			case 3:
				xx, yy = w-1-x, h-1-y
			case 4:
				yy = h - 1 - y
			case 5:
				xx, yy = y, x
			case 6:
				xx, yy = h-1-y, x
			case 7:
				xx, yy = h-1-y, w-1-x
			case 8:
				xx, yy = y, w-1-x
			}
			out.Set(xx, yy, src.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return out, nil
}

// Fit the complete oriented image inside the fixed native canvas. Integer
// arithmetic keeps both axes bounded even for a one-pixel-wide source.
func captionDimensions(w, h int) (int, int) {
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	if w >= h {
		return captionCanvasSide, max(1, int(int64(h)*captionCanvasSide/int64(w)))
	}
	return max(1, int(int64(w)*captionCanvasSide/int64(h))), captionCanvasSide
}
func resizeCaption(ctx context.Context, src image.Image) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := src.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || int64(b.Dx()) > imageMaximumPixels/int64(b.Dy()) {
		return nil, ErrLimit
	}
	w, h := captionDimensions(b.Dx(), b.Dy())
	dst := image.NewNRGBA(image.Rect(0, 0, captionCanvasSide, captionCanvasSide))
	neutral := color.NRGBA{R: 127, G: 127, B: 127, A: 255}
	for y := 0; y < captionCanvasSide; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < captionCanvasSide; x++ {
			dst.SetNRGBA(x, y, neutral)
		}
	}
	x0, y0 := (captionCanvasSide-w)/2, (captionCanvasSide-h)/2
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Preserve both source edges; when an axis is one pixel, sample its center.
		sy := (b.Dy() - 1) / 2
		if h > 1 {
			sy = int(int64(y) * int64(b.Dy()-1) / int64(h-1))
		}
		for x := 0; x < w; x++ {
			sx := (b.Dx() - 1) / 2
			if w > 1 {
				sx = int(int64(x) * int64(b.Dx()-1) / int64(w-1))
			}
			c := color.NRGBAModel.Convert(src.At(b.Min.X+sx, b.Min.Y+sy)).(color.NRGBA)
			alpha := uint32(c.A)
			c.R = uint8((uint32(c.R)*alpha + 127*(255-alpha)) / 255)
			c.G = uint8((uint32(c.G)*alpha + 127*(255-alpha)) / 255)
			c.B = uint8((uint32(c.B)*alpha + 127*(255-alpha)) / 255)
			c.A = 255
			dst.SetNRGBA(x0+x, y0+y, c)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return dst, nil
}

type imageContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r imageContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (e *Extractor) prepareImage(ctx context.Context, original *Original, caption bool, own *extractionOwnership) (*Decoded, error) {
	meta := original.Metadata()
	switch meta.Format {
	case "png", "jpeg", "webp":
	default:
		return nil, ErrUnsupported
	}
	info, err := inspectImage(original.Reader(), meta.Bytes)
	if err != nil {
		return nil, err
	}
	// Reserve the complete decoded/preprocessed extent before allocating pixels.
	maxBytes := int64(info.Width)*int64(info.Height)*4 + (2 << 20)
	if !caption {
		maxBytes += int64(info.Width) * int64(info.Height) * 3
	}
	d, err := e.options.Store.ReserveDecoded(ctx, meta.Source, meta.MediaID, maxBytes)
	if err != nil {
		return nil, err
	}
	// Ownership precedes decoding: failed validation/encoding or a failed Close
	// must retain the durable reservation and its retry owner.
	e.trackDecoded(ctx, own, d)
	workctx, cancel := context.WithTimeout(ctx, e.options.Timeout)
	defer cancel()
	src, format, err := image.Decode(imageContextReader{workctx, io.NewSectionReader(original.Reader(), 0, meta.Bytes)})
	if workctx.Err() != nil {
		return nil, workctx.Err()
	}
	if err != nil || format != info.Format {
		return nil, ErrCorrupt
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	src, err = orientImage(workctx, src, info.Orientation)
	if err != nil {
		return nil, err
	}
	if caption {
		src, err = resizeCaption(workctx, src)
		if err != nil {
			return nil, err
		}
		err = png.Encode(d.File(), src)
	} else {
		b := src.Bounds()
		header := make([]byte, 8)
		binary.LittleEndian.PutUint32(header[:4], uint32(b.Dx()))
		binary.LittleEndian.PutUint32(header[4:], uint32(b.Dy()))
		_, err = d.File().Write(header)
		row := make([]byte, b.Dx()*3)
		for y := b.Min.Y; err == nil && y < b.Max.Y; y++ {
			if err = workctx.Err(); err != nil {
				break
			}
			for x := b.Min.X; x < b.Max.X; x++ {
				c := color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
				i := (x - b.Min.X) * 3
				alpha := uint32(c.A)
				row[i] = uint8((uint32(c.R)*alpha + 255*(255-alpha)) / 255)
				row[i+1] = uint8((uint32(c.G)*alpha + 255*(255-alpha)) / 255)
				row[i+2] = uint8((uint32(c.B)*alpha + 255*(255-alpha)) / 255)
			}
			_, err = d.File().Write(row)
		}
	}
	if err != nil {
		return nil, err
	}
	if err = d.Seal(workctx); err != nil {
		return nil, err
	}
	return d, nil
}
