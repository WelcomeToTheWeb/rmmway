// Package files is the agent side of gap #1a's file transfer: FilePull
// streams a local file to the server in FileChunk uplink frames, FilePush
// writes an incoming file (inline content_b64 or chunked FileChunk
// downlink frames) to a destination path.
package files

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// MaxChunkBytes is the on-the-wire block size for FileChunk frames (the
// proto contract: 256 KiB).
const MaxChunkBytes = 256 * 1024

// MaxPullBytes bounds a single file_pull (phase 1 keeps pulled files
// in-memory on the server; 256 MiB is generous without OOM risk on the
// server side).
const MaxPullBytes = 256 * 1024 * 1024

// SendPull streams the file at path as FileChunk frames via send, in
// 256 KiB blocks (0-based seq, eof on the final chunk — sent even when the
// last block is empty). Returns the total size and the source mode.
func SendPull(ctx context.Context, cmdID, path string, send func(*agentv1.FileChunk) error) (int64, uint32, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "SendPull: stat %s size=%d\n", path, fi.Size())
	if !fi.Mode().IsRegular() {
		return 0, 0, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > MaxPullBytes {
		return 0, 0, fmt.Errorf("%s is %d bytes (phase 1 pulls are capped at %d)", path, fi.Size(), MaxPullBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var seq uint64
	buf := make([]byte, MaxChunkBytes)
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := &agentv1.FileChunk{
				CommandId:  cmdID,
				Seq:        seq,
				Data:       append([]byte(nil), buf[:n]...),
				TotalBytes: fi.Size(),
				SourceMode: uint32(fi.Mode().Perm()),
			}
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return 0, 0, rerr
			}
			if errors.Is(rerr, io.EOF) {
				chunk.Eof = true
			}
			if err := send(chunk); err != nil {
				return 0, 0, err
			}
			if chunk.Eof {
				return fi.Size(), uint32(fi.Mode().Perm()), nil
			}
			seq++
			continue
		}
		if rerr == nil {
			continue // spurious 0-byte read; loop
		}
		if errors.Is(rerr, io.EOF) {
			// Zero-byte file: still send the (empty) eof chunk so the
				// receiver finalizes the transfer.
			chunk := &agentv1.FileChunk{
				CommandId:  cmdID, Seq: seq, Eof: true,
				TotalBytes: 0, SourceMode: uint32(fi.Mode().Perm()),
			}
			if err := send(chunk); err != nil {
				return 0, 0, err
			}
			return 0, uint32(fi.Mode().Perm()), nil
		}
		return 0, 0, rerr
	}
}

// PushSession assembles one chunked file_push: chunks must arrive in
// 0-based seq order; the eof chunk finalizes the write (mode applied when
// requested, atomic rename into place).
type PushSession struct {
	cmdID, path, mode string
	tmp               *os.File
	expected          uint64
	size              int64

	mu     sync.Mutex
	done   chan struct{}
	closed bool
	err    error
}

// StartPush prepares the destination temp file. The parent directory of
// path must exist; a missing file is created, an existing one replaced.
func StartPush(cmdID, path, mode string) (*PushSession, error) {
	dir := filepath.Dir(path)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("destination directory %s does not exist", dir)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".rmmwaytmp*")
	if err != nil {
		return nil, fmt.Errorf("create temp in %s: %w", dir, err)
	}
	p := &PushSession{
		cmdID: cmdID, path: path, mode: mode,
		tmp: tmp, done: make(chan struct{}),
	}
	return p, nil
}

// Chunk accepts one FileChunk. Out-of-order or duplicate chunks are
// errors (the sender is ordered by construction); the eof chunk closes
// the session and finalizes the file.
func (p *PushSession) Chunk(c *agentv1.FileChunk) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("push %s already finished", p.cmdID)
	}
	if c.GetCommandId() != p.cmdID {
		return fmt.Errorf("chunk for command %s delivered to push %s", c.GetCommandId(), p.cmdID)
	}
	if c.GetSeq() != p.expected {
		return fmt.Errorf("chunk seq %d out of order (want %d)", c.GetSeq(), p.expected)
	}
	if len(c.GetData()) > MaxChunkBytes {
		return fmt.Errorf("chunk of %d bytes exceeds the %d limit", len(c.GetData()), MaxChunkBytes)
	}
	if _, err := p.tmp.Write(c.GetData()); err != nil {
		return err
	}
	p.size += int64(len(c.GetData()))
	p.expected++
	if c.GetEof() {
		return p.finishLocked()
	}
	return nil
}

// InlinePush writes a push whose content rides inline (no chunks) — same
// temp+rename+mode contract as the chunked path.
func InlinePush(cmdID, path, mode string, contentB64 string) error {
	content, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		return fmt.Errorf("content_b64: %w", err)
	}
	return writeContent(path, mode, content)
}

func (p *PushSession) finishLocked() error {
	p.closed = true
	if err := p.tmp.Close(); err != nil {
		p.err = err
		close(p.done)
		return err
	}
	if err := os.Rename(p.tmp.Name(), p.path); err != nil {
		os.Remove(p.tmp.Name())
		p.err = fmt.Errorf("replace %s: %w", p.path, err)
		close(p.done)
		return p.err
	}
	if p.mode != "" {
		if m, err := parseMode(p.mode); err == nil {
			if cerr := os.Chmod(p.path, m); cerr != nil {
				p.err = fmt.Errorf("chmod %s: %w", p.path, cerr)
				close(p.done)
				return p.err
			}
		} else {
			p.err = fmt.Errorf("bad mode %q: %w", p.mode, err)
		}
	}
	close(p.done)
	return nil
}

// Wait blocks until the eof chunk finalizes the file (or ctx/timeout).
// Returns the byte count written.
func (p *PushSession) Wait(ctx context.Context) (int64, error) {
	select {
	case <-p.done:
		if p.err != nil {
			return p.size, p.err
		}
		return p.size, nil
	case <-ctx.Done():
		p.mu.Lock()
		if !p.closed {
			p.closed = true
			p.tmp.Close()
			os.Remove(p.tmp.Name())
		}
		p.mu.Unlock()
		return 0, ctx.Err()
	}
}

// Abandon cleans up a push that will not complete (timeout, stream drop).
func (p *PushSession) Abandon() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.tmp.Close()
	os.Remove(p.tmp.Name())
	close(p.done)
}

func writeContent(path, mode string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".rmmwaytmp*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", filepath.Dir(path), err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if mode != "" {
		if m, err := parseMode(mode); err == nil {
			return os.Chmod(path, m)
		} else {
			return fmt.Errorf("bad mode %q: %w", mode, err)
		}
	}
	return nil
}

// parseMode accepts octal strings like "0644", "644", "0o755".
func parseMode(s string) (os.FileMode, error) {
	t := s
	switch {
	case len(t) >= 2 && t[:2] == "0o":
		t = t[2:]
	case len(t) >= 1 && t[0] == '0' && len(t) > 1:
		// "0644" — keep the leading zero; Go's ParseInt(8) handles it.
	}
	n, err := parseOctal(t)
	if err != nil {
		return 0, err
	}
	return os.FileMode(n), nil
}

func parseOctal(t string) (int64, error) {
	var n int64
	for _, r := range t {
		if r < '0' || r > '7' {
			return 0, fmt.Errorf("not octal")
		}
		n = n*8 + int64(r-'0')
	}
	return n, nil
}

// Compile-time check that the generated types are what we expect.
var _ = agentv1.FileChunk{}
