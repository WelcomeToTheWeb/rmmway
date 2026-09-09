//go:build linux

// Linux capture (gap #1a):
//   - DISPLAY unset/empty (the norm on headless boxes / CI): emit the
//     "vnc_required" status frame — opening a local VNC/X session makes
//     real frames follow without any agent change.
//   - DISPLAY set: a minimal pure-Go X11 core client — connect, handshake,
//     then per tick one GetImage on the root window (24-bit packed, capped
//     at 1280x720 for the wire). No X extensions (no MIT-SHM in phase 1);
//     one request per tick keeps it simple and safe. Any connection error
//     degrades to "unavailable" status frames — capture never takes down
//     the uplink.
package session

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	maxCapWidth  = 1280
	maxCapHeight = 720
)

func defaultCapturer() (Capturer, error) {
	return &linuxCapturer{}, nil
}

type linuxCapturer struct {
	conn    net.Conn
	ord     binary.ByteOrder
	root    uint32
	xid     uint32
	screenW int
	screenH int
}

func (c *linuxCapturer) Name() string { return "linux-x11" }

func (c *linuxCapturer) Capture(ctx context.Context) (*Frame, error) {
	display := os.Getenv("DISPLAY")
	if display == "" {
		return &Frame{Status: "vnc_required"}, nil
	}
	if err := c.ensureConn(); err != nil {
		return &Frame{Status: "unavailable"}, nil
	}
	img, err := c.getImage()
	if err != nil {
		c.resetConn()
		return &Frame{Status: "unavailable"}, nil
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("x11: encode jpeg: %w", err)
	}
	return &Frame{JPEG: buf.Bytes(), Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), TSMS: nowMS()}, nil
}

func (c *linuxCapturer) ensureConn() error {
	if c.conn != nil {
		return nil
	}
	display := os.Getenv("DISPLAY")
	host := ""
	var num int
	if i := strings.IndexByte(display, ':'); i >= 0 {
		rest := display[i+1:]
		if j := strings.IndexByte(rest, ':'); j >= 0 {
			host = rest[:j]
			rest = rest[j+1:]
		}
		var err error
		num, err = strconv.Atoi(strings.SplitN(rest, ".", 2)[0])
		if err != nil {
			return fmt.Errorf("bad DISPLAY %q", display)
		}
	}
	var (
		conn net.Conn
		err  error
	)
	if host == "" {
		conn, err = net.DialTimeout("unix", fmt.Sprintf("/tmp/.X11-unix/X%d", num), 3*time.Second)
	} else {
		conn, err = net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(6000+num)), 3*time.Second)
	}
	if err != nil {
		return fmt.Errorf("dial X display %s: %w", display, err)
	}
	// Handshake: send 0 (client byte order = least significant byte
	// first); the 8-byte reply tells us the server's byte order.
	if _, err := conn.Write([]byte{0}); err != nil {
		conn.Close()
		return err
	}
	var hdr [8]byte
	if _, err := readFull(conn, hdr[:]); err != nil {
		conn.Close()
		return err
	}
	var ord binary.ByteOrder = binary.LittleEndian
	if hdr[7] == 0 { // format: 0 = big-endian server
		ord = binary.BigEndian
	}
	// SetupReply: 8 bytes fixed header + 256 bytes fixed body + screens
	// array. Read the fixed portion (264 bytes), then peek at screens[0].
	var setup [264]byte
	if _, err := readFull(conn, setup[:]); err != nil {
		conn.Close()
		return err
	}
	// Root window of screen 0 is at offset 12 in the fixed body.
	root := ord.Uint32(setup[12:16])
	// Screens array starts after the fixed portion; width at offset 264+0.
	var screenHdr [8]byte
	if _, err := readFull(conn, screenHdr[:]); err != nil {
		conn.Close()
		return err
	}
	c.screenW = int(ord.Uint16(screenHdr[0:2]))
	c.screenH = int(ord.Uint16(screenHdr[2:4]))
	c.conn = conn
	c.ord = ord
	c.root = root
	return nil
}

func (c *linuxCapturer) resetConn() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

func (c *linuxCapturer) Close() error {
	c.resetConn()
	return nil
}

func (c *linuxCapturer) getImage() (*image.RGBA, error) {
	w, h := c.screenW, c.screenH
	if w <= 0 || w > maxCapWidth {
		w = maxCapWidth
	}
	if h <= 0 || h > maxCapHeight {
		h = maxCapHeight
	}
	c.xid++
	// GetImage request: 9 words (36 bytes)
	req := make([]byte, 36)
	ord := c.ord
	req[0] = 0
	req[1] = 78 // GetImage opcode
	ord.PutUint32(req[4:8], c.xid)
	ord.PutUint32(req[8:12], c.root)
	ord.PutUint32(req[16:20], uint32(w))
	ord.PutUint32(req[20:24], uint32(h))
	ord.PutUint32(req[24:28], 24) // depth
	ord.PutUint32(req[28:32], 24) // format
	if _, err := c.conn.Write(req); err != nil {
		return nil, err
	}
	// Read reply header (32 bytes).
	var reply [32]byte
	if _, err := readFull(c.conn, reply[:]); err != nil {
		return nil, err
	}
	if reply[0] != 1 {
		return nil, fmt.Errorf("X error reply: code %d", reply[1])
	}
	rw, rh := int(ord.Uint16(reply[20:22])), int(ord.Uint16(reply[22:24]))
	if rw != w || rh != h {
		return nil, fmt.Errorf("GetImage returned wrong size (%d,%d)", rw, rh)
	}
	// Data length follows in next 4 bytes.
	var lenBuf [4]byte
	if _, err := readFull(c.conn, lenBuf[:]); err != nil {
		return nil, err
	}
	dataLen := int(ord.Uint32(lenBuf[:])) * 4
	body := make([]byte, dataLen)
	if _, err := readFull(c.conn, body); err != nil {
		return nil, err
	}
	rowBytes := (rw*3 + 3) &^ 3
	if len(body) < rowBytes*rh {
		return nil, fmt.Errorf("short GetImage data (%d < %d)", len(body), rowBytes*rh)
	}
	pix := body
	img := image.NewRGBA(image.Rect(0, 0, rw, rh))
	for y := 0; y < rh; y++ {
		src := pix[y*rowBytes : (y+1)*rowBytes]
		dst := img.Pix[y*rw*4 : (y+1)*rw*4]
		for x := 0; x < rw; x++ {
			b, g, r := src[x*3], src[x*3+1], src[x*3+2]
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, b, 255
		}
	}
	return img, nil
}

func readFull(conn net.Conn, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := conn.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
