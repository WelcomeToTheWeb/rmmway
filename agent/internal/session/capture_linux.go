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
		// Drop the connection; the next tick re-dials (the X server may
		// have restarted, or the display may have gone away).
		c.resetConn()
		return &Frame{Status: "unavailable"}, nil
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, fmt.Errorf("x11: encode jpeg: %w", err)
	}
	return &Frame{JPEG: buf.Bytes(), Width: img.Bounds().Dx(), Height: img.Bounds().Dy(), TSMS: nowMS()}, nil
}

// ensureConn dials + handshakes the X server once per capturer lifetime.
// display strings look like ":0", ":1.0", or "host:0".
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
	ord := binary.LittleEndian
	if hdr[7] == 0 { // format: 0 = big-endian server
		ord = binary.BigEndian
	}
	var setup [264]byte
	if _, err := readFull(conn, setup[:]); err != nil {
		conn.Close()
		return err
	}
	// Setup: offset 8 = root window of screen 0; offsets 268/270 = its
	// width/height (first entry of the screens array).
	root := ord.Uint32(setup[8:12])
	c.screenW = int(ord.Uint16(setup[268:270]))
	c.screenH = int(ord.Uint16(setup[270:272]))
	c.conn, c.ord, c.root, c.xid = conn, ord, root, 0
	return nil
}

func (c *linuxCapturer) resetConn() {
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = nil
}

func (c *linuxCapturer) Close() error { return c.resetConn() }

// getImage sends one GetImage request and decodes its 24-bit reply into an
// image.RGBA (BGR triples as X depth-24 visuals pack them).
func (c *linuxCapturer) getImage() (*image.RGBA, error) {
	w, h := c.screenW, c.screenH
	if w <= 0 || w > maxCapWidth {
		w = maxCapWidth
	}
	if h <= 0 || h > maxCapHeight {
		h = maxCapHeight
	}
	c.xid++
	req := make([]byte, 36)
	ord := c.ord
	req[0] = 0 // data request
	req[1] = 78 // GetImage
	ord.PutUint32(req[2:6], 6) // data length (in 4-byte units)
	ord.PutUint32(req[4:8], c.xid)
	ord.PutUint32(req[8:12], c.root)
	ord.PutUint32(req[12:16], 0) // x
	ord.PutUint32(req[16:20], 0) // y
	ord.PutUint32(req[20:24], uint32(w))
	ord.PutUint32(req[24:28], uint32(h))
	ord.PutUint32(req[28:32], 24) // depth
	ord.PutUint32(req[32:36], 24) // format: 24-bit packed
	if _, err := c.conn.Write(req); err != nil {
		return nil, err
	}
	var lenBuf [4]byte
	if _, err := readFull(c.conn, lenBuf[:]); err != nil {
		return nil, err
	}
	dataLen := int(ord.Uint32(lenBuf[:])) * 4
	body := make([]byte, dataLen)
	if _, err := readFull(c.conn, body); err != nil {
		return nil, err
	}
	if len(body) < 20 || body[4] != 50 { // 50 = success reply type
		if len(body) > 0 && body[0] == 0 {
			return nil, fmt.Errorf("X error reply: code %d", body[1])
		}
		return nil, fmt.Errorf("short GetImage reply (%d bytes)", len(body))
	}
	rw, rh := int(ord.Uint16(body[8:10])), int(ord.Uint16(body[10:12]))
	rowBytes := (rw*3 + 3) &^ 3 // server pads scanlines to a 32-bit boundary
	if len(body) < 20+rowBytes*rh {
		return nil, fmt.Errorf("short GetImage data (%d < %d)", len(body), 20+rowBytes*rh)
	}
	pix := body[20:]
	img := image.NewRGBA(image.Rect(0, 0, rw, rh))
	for y := 0; y < rh; y++ {
		src := pix[y*rowBytes : (y+1)*rw*3]
		dst := img.Pix[y*rw*4 : (y+1)*rw*4]
		for x := 0; x < rw; x++ {
			// X depth-24 visuals are packed BGR.
			b, g, r := src[x*3], src[x*3+1], src[x*3+2]
			dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = r, g, b, 255
		}
	}
	return img, nil
}

// readFull reads exactly len(p) bytes (net.Conn is unbuffered).
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
