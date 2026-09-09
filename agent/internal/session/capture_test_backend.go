// Synthetic capture backend (gap #1a). RMMWAY_SESSION_SOURCE=test selects
// it; it renders a deterministic animated pattern (gradient background, a
// block that travels across the screen, and a frame-counter bar) so the
// whole pipeline — capture loop, SessionFrame uplink, server relay, SSE
// viewer — is e2e-testable on headless machines and in CI, where no real
// display exists. Pure Go (image/jpeg): no cgo, works in every static
// binary target.
package session

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
)

// testBackendSize keeps frames small (a few tens of KB of JPEG) — enough
// to prove the pipeline, cheap enough for e2e assertions.
const (
	testWidth  = 640
	testHeight = 360
)

type testCapturer struct {
	seq uint64
}

func newTestCapturer() *testCapturer { return &testCapturer{} }

func (t *testCapturer) Name() string { return "test" }

func (t *testCapturer) Capture(context.Context) (*Frame, error) {
	img := image.NewRGBA(image.Rect(0, 0, testWidth, testHeight))

	// Vertical gradient background (cheap: one row at a time).
	for y := 0; y < testHeight; y++ {
		r := color.RGBA{R: uint8(20 + 60*y/testHeight), G: uint8(120), B: uint8(220 - 80*y/testHeight), A: 255}
		for x := 0; x < testWidth; x++ {
			img.SetRGBA(x, y, r)
		}
	}

	// A block traveling left-to-right in a boustrophedon path, so every
	// frame is visually distinct (frame-rate + drop detection).
	s := t.seq
	bx := int((s * 13) % (testWidth - 60))
	by := int((s*7)%(testHeight-60))
	by = (by / 30) * 30
	block := image.NewUniform(color.RGBA{R: 250, G: 200, B: 40, A: 255})
	draw.Draw(img, image.Rect(bx, by, bx+60, by+30), block, image.Point{}, draw.Over)

	// Frame-counter bar along the bottom: its length grows with seq mod
	// 64, so a frozen feed is obvious in one glance.
	bar := image.NewUniform(color.RGBA{R: 40, G: 220, B: 120, A: 255})
	barLen := int(s % 64) * (testWidth / 64)
	draw.Draw(img, image.Rect(0, testHeight-8, barLen, testHeight), bar, image.Point{}, draw.Over)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("test backend: encode jpeg: %w", err)
	}
	t.seq++
	return &Frame{JPEG: buf.Bytes(), Width: testWidth, Height: testHeight, TSMS: nowMS()}, nil
}

func (t *testCapturer) Close() error { return nil }
