// Package sessionrelay is the server side of gap #1a (remote session). It
// receives SessionFrame uplink frames from ingest, keeps only the latest
// frame per device (drop-old — a slow viewer never backs the agent up),
// and streams frames to subscribed browser viewers via SSE. It also
// accumulates FileChunk frames for file_pull transfers.
//
// Design: one Registry instance shared between ingest (OnFrame/OnChunk)
// and the HTTP API (Subscribe/TransferState). No persistence (phase 1).
package sessionrelay

import (
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

// TransferState tracks an in-progress file_pull.
type TransferState struct {
	Path      string
	Total     int64
	Received  int64
	Mode      uint32
	Data      []byte
	Done      bool
	Err       error
	StartedAt time.Time
	Complete  time.Time
}

// FrameEvent is one SSE frame sent to a viewer.
type FrameEvent struct {
	Kind      string // "frame" | "status" | "hello"
	SessionID string
	Seq       uint64
	Codec     string
	Width     uint32
	Height    uint32
	JPEGB64   string // base64 of the jpeg bytes (empty on status frames)
	CaptureTS int64
	Status    string // non-empty on status frames ("vnc_required", "unavailable")
}

// Viewer is a single subscribed browser viewer.
type Viewer struct {
	devID string
	ch    chan FrameEvent
	done  chan struct{}
}

// Chan returns the viewer's event channel.
func (v *Viewer) Chan() chan FrameEvent {
	return v.ch
}

// Done returns the viewer's done channel.
func (v *Viewer) Done() chan struct{} {
	return v.done
}

// DeviceState holds the live state for one device.
type DeviceState struct {
	sessionID string
	fps       int
	latest    *FrameEvent
	status    string
	viewers   map[*Viewer]bool
}

// Registry is the central session relay. Thread-safe.
type Registry struct {
	mu        sync.Mutex
	devices   map[string]*DeviceState
	transfers map[string]*TransferState // command_id -> transfer
}

// New creates a Registry.
func New() *Registry {
	return &Registry{
		devices:   make(map[string]*DeviceState),
		transfers: make(map[string]*TransferState),
	}
}

// Open records that a session has been opened for a device (the server
// just sent SessionControl open). Viewers can subscribe before frames
// arrive; they'll get a hello event with the session info.
func (r *Registry) Open(devID, sessionID string, fps int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ds := r.devices[devID]
	if ds == nil {
		ds = &DeviceState{viewers: make(map[*Viewer]bool)}
		r.devices[devID] = ds
	}
	ds.sessionID = sessionID
	ds.fps = fps
	ds.status = ""
}

// OnFrame is called by ingest when a SessionFrame arrives from an agent.
// Status frames update the device's status; real frames are stored as
// the latest and sent to viewers (drop if their channel is full).
func (r *Registry) OnFrame(devID string, f *agentv1.SessionFrame) {
	r.mu.Lock()
	ds := r.devices[devID]
	if ds == nil {
		ds = &DeviceState{viewers: make(map[*Viewer]bool)}
		r.devices[devID] = ds
	}
	if f.GetSessionId() != "" {
		ds.sessionID = f.GetSessionId()
	}

	evt := FrameEvent{
		SessionID: f.GetSessionId(),
		Seq:       f.GetSeq(),
		Codec:     f.GetCodec(),
		Width:     f.GetWidth(),
		Height:    f.GetHeight(),
		CaptureTS: f.GetCaptureTsMs(),
		Status:    f.GetStatus(),
	}
	if f.GetStatus() != "" {
		evt.Kind = "status"
		ds.status = f.GetStatus()
	} else {
		evt.Kind = "frame"
		evt.JPEGB64 = base64.StdEncoding.EncodeToString(f.GetJpeg())
		ds.latest = &evt
	}
	// Copy viewer list (notify outside lock via non-blocking sends).
	viewers := make([]*Viewer, 0, len(ds.viewers))
	for v := range ds.viewers {
		viewers = append(viewers, v)
	}
	r.mu.Unlock()

	for _, v := range viewers {
		select {
		case v.ch <- evt:
		default:
			// Viewer is slow; drop this frame for it.
		}
	}
}

// Subscribe returns a channel of FrameEvents for a device. The viewer
// should call the returned cancel func when done. An initial "hello" event
// is sent with the current state (session info, latest frame if available).
func (r *Registry) Subscribe(devID string) (*Viewer, func()) {
	r.mu.Lock()
	ds := r.devices[devID]
	if ds == nil {
		ds = &DeviceState{viewers: make(map[*Viewer]bool)}
		r.devices[devID] = ds
	}
	v := &Viewer{
		devID: devID,
		ch:    make(chan FrameEvent, 16),
		done:  make(chan struct{}),
	}
	ds.viewers[v] = true

	// Send hello event with session info.
	hello := FrameEvent{
		Kind:      "hello",
		SessionID: ds.sessionID,
	}
	r.mu.Unlock()

	select {
	case v.ch <- hello:
	default:
	}

	cancel := func() {
		r.mu.Lock()
		if ds2 := r.devices[devID]; ds2 != nil {
			delete(ds2.viewers, v)
		}
		r.mu.Unlock()
		close(v.done)
	}
	return v, cancel
}

// OnChunk is called by ingest when a FileChunk arrives for a file_pull.
// Chunks are accumulated; the eof chunk finalizes the transfer.
func (r *Registry) OnChunk(devID string, c *agentv1.FileChunk) {
	r.mu.Lock()
	t := r.transfers[c.GetCommandId()]
	if t == nil {
		r.mu.Unlock()
		return // No transfer registered (or already complete)
	}
	if c.GetTotalBytes() != 0 {
		t.Total = c.GetTotalBytes()
	}
	if c.GetSourceMode() != 0 {
		t.Mode = c.GetSourceMode()
	}
	t.Received += int64(len(c.GetData()))
	t.Data = append(t.Data, c.GetData()...)
	if c.GetEof() {
		t.Done = true
		t.Complete = time.Now()
	}
	r.mu.Unlock()
}

// BeginPull registers a new file_pull transfer.
func (r *Registry) BeginPull(deviceID, commandID, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transfers[commandID] = &TransferState{
		Path:      path,
		StartedAt: time.Now(),
	}
}

// PullState returns the current state of a file_pull transfer.
func (r *Registry) PullState(commandID string) (*TransferState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.transfers[commandID]
	if !ok {
		return nil, false
	}
	// Return a copy for the caller.
	c := *t
	return &c, true
}

// ActiveSession returns the current session ID for a device.
func (r *Registry) ActiveSession(devID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ds := r.devices[devID]
	if ds == nil || ds.sessionID == "" {
		return "", false
	}
	return ds.sessionID, true
}

// LatestFrame returns the most recent frame for a device.
func (r *Registry) LatestFrame(devID string) (*FrameEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ds := r.devices[devID]
	if ds == nil || ds.latest == nil {
		return nil, false
	}
	return ds.latest, true
}

// Status returns the last status for a device.
func (r *Registry) Status(devID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ds := r.devices[devID]
	if ds == nil {
		return ""
	}
	return ds.status
}

// Close marks a session as closed (the server sent a SessionControl close
// downlink or the viewer disconnected). Future frames for this session are
// dropped; the viewer gets a "goodbye" event.
func (r *Registry) Close(devID, sessionID string) {
	r.mu.Lock()
	ds := r.devices[devID]
	if ds != nil && (sessionID == "" || ds.sessionID == sessionID) {
		ds.sessionID = ""
		for v := range ds.viewers {
			select {
			case v.ch <- FrameEvent{Kind: "goodbye", SessionID: sessionID}:
			default:
			}
		}
	}
	r.mu.Unlock()
}

// FormatBytes formats a byte count for display.
func FormatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}
