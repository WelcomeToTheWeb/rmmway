//go:build windows

// Windows input relay using the SendInput API.
//
// Mouse coordinates are absolute screen coordinates from the server (origin
// top-left) and are converted to SendInput's 0-65535 normalized space.
// Keyboard events use VK codes for modifiers and VkKeyScanW for character
// mapping. Each event is sent synchronously and SendInput's return value
// is checked to detect injection failures.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"syscall"
	"unsafe"
)

// SendInput constants.
const (
	INPUT_MOUSE    = 0
	INPUT_KEYBOARD = 1

	MOUSEEVENTF_MOVE       = 0x0001
	MOUSEEVENTF_ABSOLUTE   = 0x8000
	MOUSEEVENTF_LEFTDOWN   = 0x0002
	MOUSEEVENTF_LEFTUP     = 0x0004
	MOUSEEVENTF_RIGHTDOWN  = 0x0008
	MOUSEEVENTF_RIGHTUP    = 0x0010
	MOUSEEVENTF_MIDDLEDOWN = 0x0020
	MOUSEEVENTF_MIDDLEUP   = 0x0040
	MOUSEEVENTF_WHEEL      = 0x0800

	KEYEVENTF_KEYUP = 0x0002

	// Virtual-key codes for modifiers.
	VK_LSHIFT   = 0xA0
	VK_LCONTROL = 0xA2
	VK_LMENU    = 0xA4
	VK_LWIN     = 0x5B
)

// user32 is declared in capture_windows.go; we reuse it here via
// syscall.NewLazyDLL in this file (each .go file needs its own declaration).
var (
	inputUser32    = syscall.NewLazyDLL("user32.dll")
	procSendInput  = inputUser32.NewProc("SendInput")
	procVkKeyScanW = inputUser32.NewProc("VkKeyScanW")
)

// INPUT is the Windows INPUT union (simplified for mouse/keyboard).
type INPUT struct {
	Type     uint32
	Mouse    MOUSEINPUT
	Keyboard KEYBDINPUT
	Hardware [4]uint32 // Padding for HARDWAREINPUT
}

// MOUSEINPUT represents a mouse event.
type MOUSEINPUT struct {
	Dx        int32
	Dy        int32
	MouseData uint32
	DwFlags   uint32
	Time      uint32
	ExtraInfo uintptr
}

// KEYBDINPUT represents a keyboard event.
type KEYBDINPUT struct {
	Vk        uint16
	Scan      uint16
	DwFlags   uint32
	Time      uint32
	ExtraInfo uintptr
}

// windowsInputRelayer uses the Windows SendInput API.
type windowsInputRelayer struct {
	logger  *slog.Logger
	screenW int32
	screenH int32
}

func newPlatformInputRelayer(cfg InputRelayerConfig) (InputRelayer, error) {
	r := &windowsInputRelayer{
		logger: cfg.Logger,
	}
	// Get screen dimensions via GetSystemMetrics (declared in capture_windows.go).
	// We call it via our own syscall declaration since capture_windows.go's
	// variable isn't package-visible.
	getSM := syscall.NewLazyDLL("user32.dll").NewProc("GetSystemMetrics")
	w, _, _ := getSM.Call(0) // SM_CXSCREEN
	h, _, _ := getSM.Call(1) // SM_CYSCREEN
	r.screenW = int32(w)
	r.screenH = int32(h)
	if r.screenW == 0 || r.screenH == 0 {
		return nil, fmt.Errorf("GetSystemMetrics failed")
	}
	r.logger.Debug("session: input relayer ready", "screen", fmt.Sprintf("%dx%d", r.screenW, r.screenH))
	return r, nil
}

// sendInput calls SendInput with one event and verifies it was injected.
func (r *windowsInputRelayer) sendInput(inp *INPUT) error {
	r1, _, err := procSendInput.Call(
		1,
		uintptr(unsafe.Pointer(inp)),
		uintptr(unsafe.Sizeof(*inp)),
	)
	if r1 != 1 {
		return fmt.Errorf("SendInput reported 0 events injected (wanted 1): %w", err)
	}
	if err != nil && err != syscall.Errno(0) {
		return fmt.Errorf("SendInput syscall: %w", err)
	}
	return nil
}

// normalize maps an absolute screen coordinate to SendInput's 0-65535 range.
func (r *windowsInputRelayer) normalizeX(x int) uint16 {
	n := (int64(x) * 65535) / int64(r.screenW)
	if n > 65535 {
		n = 65535
	}
	return uint16(n)
}

func (r *windowsInputRelayer) normalizeY(y int) uint16 {
	n := (int64(y) * 65535) / int64(r.screenH)
	if n > 65535 {
		n = 65535
	}
	return uint16(n)
}

// buttonToFlags maps a button number to SendInput mouse button flag constants.
func buttonToFlags(button int) (down, up uint32) {
	switch button {
	case 0:
		return MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP
	case 1:
		return MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_MIDDLEUP
	case 2:
		return MOUSEEVENTF_RIGHTDOWN, MOUSEEVENTF_RIGHTUP
	default:
		return MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP
	}
}

func (r *windowsInputRelayer) MouseEvent(ctx context.Context, eventType string, x, y, button, wheelDelta int) error {
	switch eventType {
	case "move":
		return r.mouseMove(x, y)
	case "down":
		downFlags, _ := buttonToFlags(button)
		return r.mouseButton(x, y, downFlags, false)
	case "up":
		_, upFlags := buttonToFlags(button)
		return r.mouseButton(x, y, upFlags, false)
	case "click":
		downFlags, upFlags := buttonToFlags(button)
		if err := r.mouseButton(x, y, downFlags, false); err != nil {
			return err
		}
		return r.mouseButton(x, y, upFlags, false)
	case "wheel":
		return r.mouseWheel(x, y, wheelDelta)
	default:
		return fmt.Errorf("unknown mouse event type: %s", eventType)
	}
}

func (r *windowsInputRelayer) mouseMove(x, y int) error {
	r.logger.Debug("session: mouse move", "x", x, "y", y)
	var inp INPUT
	inp.Type = INPUT_MOUSE
	inp.Mouse = MOUSEINPUT{
		Dx:      int32(r.normalizeX(x)),
		Dy:      int32(r.normalizeY(y)),
		DwFlags: MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE,
	}
	return r.sendInput(&inp)
}

func (r *windowsInputRelayer) mouseButton(x, y int, flags uint32, _ bool) error {
	var inp INPUT
	inp.Type = INPUT_MOUSE
	inp.Mouse = MOUSEINPUT{
		Dx:      int32(r.normalizeX(x)),
		Dy:      int32(r.normalizeY(y)),
		DwFlags: flags | MOUSEEVENTF_ABSOLUTE,
	}
	return r.sendInput(&inp)
}

func (r *windowsInputRelayer) mouseWheel(x, y, delta int) error {
	r.logger.Debug("session: mouse wheel", "delta", delta)
	var inp INPUT
	inp.Type = INPUT_MOUSE
	inp.Mouse = MOUSEINPUT{
		Dx:        int32(r.normalizeX(x)),
		Dy:        int32(r.normalizeY(y)),
		DwFlags:   MOUSEEVENTF_WHEEL | MOUSEEVENTF_ABSOLUTE,
		MouseData: uint32(delta),
	}
	return r.sendInput(&inp)
}

// vkForCodepoint maps a Unicode codepoint to a virtual-key code via VkKeyScanW,
// applying the current keyboard layout. Returns (vk, shift) where shift is true
// if the shift key must be held.
func (r *windowsInputRelayer) vkForCodepoint(cp uint32) (uint8, bool) {
	r1, _, _ := procVkKeyScanW.Call(uintptr(cp))
	if r1 == 0xFFFF {
		return 0, false
	}
	vk := byte(r1)
	shift := (byte(r1>>8) & 1) != 0
	return vk, shift
}

func (r *windowsInputRelayer) KeyboardEvent(ctx context.Context, eventType string, codepoint uint32, modifiers uint32) error {
	switch eventType {
	case "down":
		return r.keyDown(codepoint, modifiers)
	case "up":
		return r.keyUp(codepoint, modifiers)
	case "key":
		// Single press = down then up.
		if err := r.keyDown(codepoint, modifiers); err != nil {
			return err
		}
		return r.keyUp(codepoint, modifiers)
	default:
		return fmt.Errorf("unknown keyboard event type: %s", eventType)
	}
}

// sendModifier sends down/up for a modifier virtual-key.
func (r *windowsInputRelayer) sendModifier(vk uint8, down bool) error {
	var inp INPUT
	inp.Type = INPUT_KEYBOARD
	inp.Keyboard = KEYBDINPUT{Vk: uint16(vk)}
	if !down {
		inp.Keyboard.DwFlags = KEYEVENTF_KEYUP
	}
	return r.sendInput(&inp)
}

func (r *windowsInputRelayer) keyDown(cp uint32, mods uint32) error {
	r.logger.Debug("session: key down", "codepoint", cp, "modifiers", mods)
	// Send modifier keys first.
	if mods&1 != 0 {
		if err := r.sendModifier(VK_LSHIFT, true); err != nil {
			return err
		}
	}
	if mods&2 != 0 {
		if err := r.sendModifier(VK_LCONTROL, true); err != nil {
			return err
		}
	}
	if mods&4 != 0 {
		if err := r.sendModifier(VK_LMENU, true); err != nil {
			return err
		}
	}
	if mods&8 != 0 {
		if err := r.sendModifier(VK_LWIN, true); err != nil {
			return err
		}
	}
	if cp == 0 {
		return nil
	}
	vk, needShift := r.vkForCodepoint(cp)
	if needShift {
		if err := r.sendModifier(VK_LSHIFT, true); err != nil {
			return err
		}
	}
	var inp INPUT
	inp.Type = INPUT_KEYBOARD
	inp.Keyboard = KEYBDINPUT{Vk: uint16(vk)}
	return r.sendInput(&inp)
}

func (r *windowsInputRelayer) keyUp(cp uint32, mods uint32) error {
	r.logger.Debug("session: key up", "codepoint", cp, "modifiers", mods)
	if cp != 0 {
		vk, _ := r.vkForCodepoint(cp)
		var inp INPUT
		inp.Type = INPUT_KEYBOARD
		inp.Keyboard = KEYBDINPUT{Vk: uint16(vk), DwFlags: KEYEVENTF_KEYUP}
		if err := r.sendInput(&inp); err != nil {
			return err
		}
	}
	// Release modifiers in reverse order.
	if mods&8 != 0 {
		if err := r.sendModifier(VK_LWIN, false); err != nil {
			return err
		}
	}
	if mods&4 != 0 {
		if err := r.sendModifier(VK_LMENU, false); err != nil {
			return err
		}
	}
	if mods&2 != 0 {
		if err := r.sendModifier(VK_LCONTROL, false); err != nil {
			return err
		}
	}
	if mods&1 != 0 {
		if err := r.sendModifier(VK_LSHIFT, false); err != nil {
			return err
		}
	}
	return nil
}

func (r *windowsInputRelayer) Close() error {
	return nil
}
