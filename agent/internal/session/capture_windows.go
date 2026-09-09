//go:build windows

// Windows capture (gap #1a): GDI BitBlt of the primary screen via raw
// syscall (no cgo — the agent stays a static binary). GetDC(0) →
// compatible DC/bitmap → BitBlt(SRCCOPY|CAPTUREBLT) → GetBitmapBits
// (32-bit BGRA, bottom-up) → flip to top-down RGBA → JPEG.
package session

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"unsafe"

	"syscall"
)

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	gdi32                    = syscall.NewLazyDLL("gdi32.dll")
	procGetDC                = user32.NewProc("GetDC")
	procReleaseDC            = user32.NewProc("ReleaseDC")
	procCreateCompatibleDC   = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatBitmap   = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject         = gdi32.NewProc("SelectObject")
	procBitBlt               = gdi32.NewProc("BitBlt")
	procDeleteObject         = gdi32.NewProc("DeleteObject")
	procDeleteDC             = gdi32.NewProc("DeleteDC")
	procGetBitmapBits        = gdi32.NewProc("GetBitmapBits")
	procGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
)

const (
	smCxScreen = 0
	smCyScreen = 1

	srccopy    = 0x00CC0020
	captureBlt = 0x40000000
)

func defaultCapturer() (Capturer, error) {
	return &windowsCapturer{}, nil
}

type windowsCapturer struct{}

func (w *windowsCapturer) Name() string { return "windows-gdi" }

func (w *windowsCapturer) Capture(_ context.Context) (*Frame, error) {
	cx, _, _ := procGetSystemMetrics.Call(uintptr(smCxScreen))
	cy, _, _ := procGetSystemMetrics.Call(uintptr(smCyScreen))
	if cx == 0 || cy == 0 {
		return &Frame{Status: "unavailable"}, nil
	}

	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return &Frame{Status: "unavailable"}, nil
	}
	defer procReleaseDC.Call(0, screenDC)

	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return &Frame{Status: "unavailable"}, nil
	}
	defer procDeleteDC.Call(memDC)

	bitmap, _, _ := procCreateCompatBitmap.Call(screenDC, cx, cy)
	if bitmap == 0 {
		return &Frame{Status: "unavailable"}, nil
	}
	defer procDeleteObject.Call(bitmap)
	old, _, _ := procSelectObject.Call(memDC, bitmap)
	if old != 0 {
		defer procSelectObject.Call(memDC, old)
	}

	const bltOK = 1
	if ok, _, _ := procBitBlt.Call(memDC, 0, 0, cx, cy, screenDC, 0, 0,
		uintptr(srccopy|captureBlt)); ok == 0 {
		return &Frame{Status: "unavailable"}, nil
	}

	size := cx * cy * 4
	bits := make([]byte, size)
	if got, _, _ := procGetBitmapBits.Call(bitmap, uintptr(size),
		uintptr(unsafe.Pointer(&bits[0]))); got != uintptr(size) {
		return &Frame{Status: "unavailable"}, nil
	}

	// Bottom-up BGRA → top-down RGBA.
	img := image.NewRGBA(image.Rect(0, 0, int(cx), int(cy)))
	wi := int(cx)
	hi := int(cy)
	for y := 0; y < hi; y++ {
		src := bits[(hi-1-y)*wi*4 : (hi-y)*wi*4]
		dst := img.Pix[y*wi*4 : (y+1)*wi*4]
		for x := 0; x < wi*4; x += 4 {
			b, g, r := src[x], src[x+1], src[x+2]
			dst[x], dst[x+1], dst[x+2], dst[x+3] = r, g, b, 255
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("windows: encode jpeg: %w", err)
	}
	return &Frame{JPEG: buf.Bytes(), Width: wi, Height: hi, TSMS: nowMS()}, nil
}

func (w *windowsCapturer) Close() error { return nil }
