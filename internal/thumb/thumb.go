package thumb

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"

	_ "image/gif" // registered so a GIF preview decodes rather than 500s
)

// MaxDimension is the long edge of a derived thumbnail.
//
// Sized for the library grid at 2x on a high-DPI screen and no larger. The
// point of this file is that a page of forty cards is a few hundred kilobytes
// instead of tens of megabytes -- which matters most on the phone over the
// tailnet, the client least able to absorb the difference.
const MaxDimension = 512

// jpegQuality trades a little fidelity for a lot of bytes. At 512px this is
// visually indistinguishable from 95 and roughly half the size.
const jpegQuality = 82

// ErrTooSmall means the source is already thumbnail-sized, so deriving a copy
// would spend disk to save nothing.
var ErrTooSmall = errors.New("thumb: image is already small enough")

// Thumbnail is a derived grid image.
type Thumbnail struct {
	Data          []byte
	MIME          string
	Width, Height int

	// SourceWidth and SourceHeight describe the original, recorded so the UI
	// can reserve the right aspect box before the full image loads.
	SourceWidth, SourceHeight int
}

// Derive scales an image down to fit MaxDimension.
//
// Returns ErrTooSmall when the source already fits: most Civitai previews are
// modest, and storing a second near-identical copy of each would double the
// blob store for no benefit.
func Derive(data []byte) (*Thumbnail, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("thumb: decoding image: %w", err)
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, errors.New("thumb: image has no extent")
	}
	if sw <= MaxDimension && sh <= MaxDimension {
		return nil, ErrTooSmall
	}

	tw, th := sw, sh
	if sw >= sh {
		tw = MaxDimension
		th = sh * MaxDimension / sw
	} else {
		th = MaxDimension
		tw = sw * MaxDimension / sh
	}
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	scale(dst, src)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("thumb: encoding thumbnail: %w", err)
	}
	return &Thumbnail{
		Data: buf.Bytes(), MIME: "image/jpeg",
		Width: tw, Height: th, SourceWidth: sw, SourceHeight: sh,
	}, nil
}

// Dimensions reports an image's size without producing a thumbnail.
func Dimensions(data []byte) (int, int, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// scale is a box filter: each destination pixel averages the source pixels it
// covers.
//
// Nearest-neighbour would be a line shorter and looks visibly wrong on the
// diagonal detail these images are full of; a Lanczos resampler would look
// marginally better and needs a dependency. Averaging is the honest middle,
// and it is what makes downscaling a 2048px render legible at 512.
func scale(dst *image.RGBA, src image.Image) {
	sb := src.Bounds()
	db := dst.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw, dh := db.Dx(), db.Dy()

	// An upscale (or a same-size copy) has nothing to average, so fall back to
	// a straight draw rather than sampling a single pixel per box.
	if dw >= sw || dh >= sh {
		draw.Draw(dst, db, src, sb.Min, draw.Src)
		return
	}

	for y := 0; y < dh; y++ {
		y0 := sb.Min.Y + y*sh/dh
		y1 := sb.Min.Y + (y+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := sb.Min.X + x*sw/dw
			x1 := sb.Min.X + (x+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					pr, pg, pb, pa := src.At(sx, sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					b += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, rgba8(r/n, g/n, b/n, a/n))
		}
	}
}

// rgba8 narrows the 16-bit values RGBA() returns back to 8 bits per channel.
func rgba8(r, g, b, a uint64) color.RGBA {
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

// newZlibReader and copyN exist so png.go can decompress a zTXt chunk without
// importing compress/zlib and io itself.
func newZlibReader(r io.Reader) (io.ReadCloser, error) { return zlib.NewReader(r) }

func copyN(dst io.Writer, src io.Reader, n int64) (int64, error) {
	written, err := io.Copy(dst, io.LimitReader(src, n))
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return written, err
}

// EncodePNG re-encodes an image as PNG. Used when a caller needs a lossless
// copy of a derived image.
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
