package api

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"

	_ "image/gif" // decode-only: a GIF image part must still scale under "low"
)

// imageDetailLowSide is OpenAI's "low" contract: the model is given the image
// scaled so that its longest side is 512 px, which is also why "low" is the cheap
// option. That contract is reproducible here even though the channel has no level
// knob — the level lives in the bytes we upload, so that is where we apply it.
const imageDetailLowSide = 512

// defaultMaxImagePixels bounds what one client image may ask this process to
// decode. A 16 MiB request body can carry a PNG whose header declares hundreds of
// millions of pixels, and a decoded image costs 4 bytes per pixel: measured before
// this bound existed, a 198 KB PNG decoded to 64 MP and took the container from
// 559 MiB to a 680 MiB peak, with no memory limit on the container to stop it. 40 MP
// (≈160 MB RGBA) is far above any real chat image and far below a bomb; a larger one
// is uploaded as it arrived, and the reason is logged.
const defaultMaxImagePixels = 40_000_000

// applyImageDetail converts image_url.detail — a field of the external OpenAI
// surface — into something the channel can actually be asked to do. The channel's
// parse call takes only fileUrl/objectName/model, so there is no upstream level to
// forward; instead the level is applied to the pixels on their way up:
//
//   - "" (absent), "auto", "high": the client's intent is the channel's own
//     behaviour — full resolution — so the bytes are uploaded as they arrived.
//   - "low": downscaled so the longest side is 512 px, re-encoded (JPEG stays JPEG
//     so the upload does not grow; anything else becomes PNG, lossless).
//
// A format this package cannot decode (webp — decoding it would cost a dependency we
// do not take) is uploaded at full resolution instead. That is the one case where the
// level does not take effect, so the caller logs it rather than passing the same
// image off as scaled.
//
// It returns the bytes to upload, the name to give them (the platform keys on the
// extension), and why the level was not applied when it was not — the caller logs
// that reason. An empty note means the level took effect (or had nothing to do),
// which is the case a client can rely on.
//
// The header is read before anything is decoded: dimensions come from the image
// header, and an image past maxPixels is never turned into pixels here at all.
func applyImageDetail(data []byte, name, detail string, maxPixels int) ([]byte, string, string) {
	if !strings.EqualFold(strings.TrimSpace(detail), "low") {
		return data, name, ""
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, name, "format not decodable here"
	}
	if maxPixels > 0 && int64(cfg.Width)*int64(cfg.Height) > int64(maxPixels) {
		return data, name, fmt.Sprintf("%dx%d is %.0f megapixels, over the %d MP decode bound",
			cfg.Width, cfg.Height, float64(cfg.Width)*float64(cfg.Height)/1e6, maxPixels/1_000_000)
	}
	if cfg.Width <= imageDetailLowSide && cfg.Height <= imageDetailLowSide {
		return data, name, "" // already within low's budget: re-encoding would only lose
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, name, "format not decodable here"
	}
	scaled := boxScale(img, imageDetailLowSide)
	var buf bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: 85}); err != nil {
			return data, name, "re-encode failed"
		}
		return buf.Bytes(), withExt(name, ".jpg"), ""
	}
	if err := png.Encode(&buf, scaled); err != nil {
		return data, name, "re-encode failed"
	}
	return buf.Bytes(), withExt(name, ".png"), ""
}

// boxScale downscales by averaging each destination pixel's source box. No
// interpolation dependency, and averaging is exactly the right operator when the
// destination is smaller than the source (unlike nearest-neighbour, it does not
// alias away thin strokes — which matters for the text-in-image case).
func boxScale(src image.Image, longest int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || longest <= 0 {
		return src
	}
	nw, nh := longest, longest
	if w >= h {
		nh = int(math.Round(float64(h) * float64(longest) / float64(w)))
	} else {
		nw = int(math.Round(float64(w) * float64(longest) / float64(h)))
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					r, g, bl, a := src.At(xx, yy).RGBA()
					sr, sg, sb, sa, n = sr+uint64(r), sg+uint64(g), sb+uint64(bl), sa+uint64(a), n+1
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(sr / n >> 8), G: uint8(sg / n >> 8),
				B: uint8(sb / n >> 8), A: uint8(sa / n >> 8),
			})
		}
	}
	return dst
}

// withExt replaces a name's extension, keeping the rest (the platform keys on it).
func withExt(name, ext string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[:i] + ext
	}
	return name + ext
}
