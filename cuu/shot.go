package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"os"
)

// Inline screenshot support: get_app_state(include_screenshot:true) attaches
// the capture to the MCP response as a real image content block, so clients
// that render images (any MCP host, not just ZCode) see the window without a
// second file-read turn. Retina window captures are large; they are
// downscaled to a vision-model-friendly bound before encoding. Stdlib only —
// the binary stays dependency-free.

const inlineShotMaxDim = 1568

// downscaleBox shrinks img so max(w,h) <= maxDim using a box filter —
// adequate for screenshots (text stays legible at this bound) and free of
// external deps. Images already within the bound pass through.
func downscaleBox(img image.Image, maxDim int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return img
	}
	scale := float64(maxDim) / float64(w)
	if h > w {
		scale = float64(maxDim) / float64(h)
	}
	// round, don't truncate: 3000 * (1568/3000) is 1567.999… in floats
	nw, nh := int(float64(w)*scale+0.5), int(float64(h)*scale+0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		sy0 := y * h / nh
		sy1 := (y + 1) * h / nh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < nw; x++ {
			sx0 := x * w / nw
			sx1 := (x + 1) * w / nw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					pr, pg, pb, pa := img.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					r += uint64(pr)
					g += uint64(pg)
					bl += uint64(pb)
					a += uint64(pa)
					n++
				}
			}
			i := out.PixOffset(x, y)
			out.Pix[i+0] = uint8(r / n >> 8)
			out.Pix[i+1] = uint8(g / n >> 8)
			out.Pix[i+2] = uint8(bl / n >> 8)
			out.Pix[i+3] = uint8(a / n >> 8)
		}
	}
	return out
}

// inlineScreenshot reads a capture, downscales it, and returns base64 PNG
// data for an MCP image content block. Failures return "" — the text payload
// (with the on-disk path) is already complete, so inlining is best-effort.
func inlineScreenshot(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		logEvent("inline_shot_failed", map[string]any{"error": err.Error()})
		return ""
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, downscaleBox(img, inlineShotMaxDim)); err != nil {
		logEvent("inline_shot_failed", map[string]any{"error": err.Error()})
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
