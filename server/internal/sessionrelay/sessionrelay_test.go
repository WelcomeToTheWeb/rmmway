package sessionrelay

import (
	"testing"
	"time"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
)

func TestFrameRelay(t *testing.T) {
	r := New()

	// Open a session.
	r.Open("dev-1", "sess-1", 2)
	if sid, ok := r.ActiveSession("dev-1"); !ok || sid != "sess-1" {
		t.Fatalf("ActiveSession: got %v, %v; want sess-1, true", sid, ok)
	}

	// Send a frame.
	r.OnFrame("dev-1", &agentv1.SessionFrame{
		SessionId:   "sess-1",
		Seq:         1,
		Codec:       "jpeg",
		Width:       640,
		Height:      480,
		Jpeg:        []byte{1, 2, 3},
		CaptureTsMs: 123456,
	})

	// Latest frame should be available.
	f, ok := r.LatestFrame("dev-1")
	if !ok {
		t.Fatal("LatestFrame: no frame")
	}
	if f.Seq != 1 || f.Width != 640 {
		t.Fatalf("LatestFrame: got seq=%d width=%d", f.Seq, f.Width)
	}

	// A second frame should drop the first (drop-old).
	r.OnFrame("dev-1", &agentv1.SessionFrame{
		SessionId: "sess-1",
		Seq:       2,
		Jpeg:      []byte{4, 5, 6},
	})
	f, _ = r.LatestFrame("dev-1")
	if f.Seq != 2 {
		t.Fatalf("drop-old failed: got seq=%d", f.Seq)
	}

	// Status frames update status, not latest.
	r.OnFrame("dev-1", &agentv1.SessionFrame{
		SessionId: "sess-1",
		Status:    "vnc_required",
	})
	if s := r.Status("dev-1"); s != "vnc_required" {
		t.Fatalf("Status: got %q", s)
	}
	f, _ = r.LatestFrame("dev-1")
	if f.Seq != 2 {
		t.Fatalf("status frame overwrote latest: seq=%d", f.Seq)
	}

	// Close.
	r.Close("dev-1", "sess-1")
	if _, ok := r.ActiveSession("dev-1"); ok {
		t.Fatal("ActiveSession after Close: should be false")
	}
}

func TestViewerSubscription(t *testing.T) {
	r := New()
	r.Open("dev-2", "sess-2", 0)

	v, cancel := r.Subscribe("dev-2")
	defer cancel()

	// Should receive hello event.
	select {
	case evt := <-v.Chan():
		if evt.Kind != "hello" {
			t.Fatalf("expected hello event, got %s", evt.Kind)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for hello event")
	}

	// Frame should be delivered to viewer.
	r.OnFrame("dev-2", &agentv1.SessionFrame{
		SessionId: "sess-2",
		Seq:       1,
		Jpeg:      []byte{1, 2, 3},
	})
	select {
	case evt := <-v.Chan():
		if evt.Kind != "frame" || evt.Seq != 1 {
			t.Fatalf("unexpected event: %v", evt)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for frame event")
	}
}

func TestFileTransfer(t *testing.T) {
	r := New()

	r.BeginPull("dev-3", "cmd-1", "/etc/hosts")

	// No chunks yet — in progress.
	tf, ok := r.PullState("cmd-1")
	if !ok {
		t.Fatal("PullState: transfer not found")
	}
	if tf.Done {
		t.Fatal("transfer should not be done yet")
	}
	if tf.Path != "/etc/hosts" {
		t.Fatalf("path mismatch: %q", tf.Path)
	}

	// Send chunks.
	r.OnChunk("dev-3", &agentv1.FileChunk{
		CommandId:  "cmd-1",
		Seq:        0,
		Data:       []byte("hello "),
		TotalBytes: 11,
	})
	r.OnChunk("dev-3", &agentv1.FileChunk{
		CommandId: "cmd-1",
		Seq:       1,
		Data:      []byte("world"),
		Eof:       true,
	})

	// Should be done.
	tf, _ = r.PullState("cmd-1")
	if !tf.Done {
		t.Fatal("transfer should be done after eof chunk")
	}
	if tf.Received != 11 {
		t.Fatalf("received mismatch: %d", tf.Received)
	}
	if string(tf.Data) != "hello world" {
		t.Fatalf("data mismatch: %q", string(tf.Data))
	}

	// Unknown transfer.
	if _, ok := r.PullState("cmd-999"); ok {
		t.Fatal("PullState should not find unknown command")
	}
}

func TestFormatBytes(t *testing.T) {
	if got := FormatBytes(100); got != "100 B" {
		t.Fatalf("FormatBytes(100) = %q", got)
	}
	if got := FormatBytes(1024 * 500); got != "500.0 KB" {
		t.Fatalf("FormatBytes(512000) = %q", got)
	}
}
