import { useEffect, useRef, useState, useCallback } from "react";
import { api } from "./api.js";
import { Button, Icon, Banner } from "./ui/index.js";
import "./views/session-viewer.css";

// SessionViewer renders a live remote session for one device.
// Connects to the SSE stream and renders frames on a canvas.
export default function SessionViewer({
  deviceID,
  deviceName,
  token,
  onClose,
  onUnauthorized,
}) {
  const canvasRef = useRef(null);
  const ctxRef = useRef(null);
  const streamRef = useRef(null);
  const sessionIDRef = useRef(null);

  const [status, setStatus] = useState("connecting");
  const [error, setError] = useState(null);
  const [connected, setConnected] = useState(false);
  const [frameCount, setFrameCount] = useState(0);
  const [lastFrameTime, setLastFrameTime] = useState(null);

  // Connect to the SSE stream when mounted.
  useEffect(() => {
    if (!canvasRef.current) return;
    ctxRef.current = canvasRef.current.getContext("2d");

    let cancelled = false;

    // Start the session via API.
    api
      .startSession(token, deviceID, { fps: 2 })
      .then((res) => {
        if (cancelled) return;
        sessionIDRef.current = res.session_id;
        setStatus("connecting");
        connectStream();
      })
      .catch((e) => {
        if (cancelled) return;
        if (e.unauthorized) onUnauthorized();
        else setError(e.message);
      });

    function connectStream() {
      const evtSource = new EventSource(
        `/api/devices/${deviceID}/session/stream?token=${token}`,
      );
      streamRef.current = evtSource;

      evtSource.onopen = () => {
        if (cancelled) return;
        setConnected(true);
        setStatus("streaming");
      };

      evtSource.onmessage = (evt) => {
        if (cancelled) return;
        try {
          const data = JSON.parse(evt.data);
          handleFrameEvent(data);
        } catch (e) {
          // Ignore parse errors
        }
      };

      evtSource.onerror = (e) => {
        if (cancelled) return;
        evtSource.close();
        setConnected(false);
        if (status !== "stopped") {
          setStatus("disconnected");
        }
      };
    }

    function handleFrameEvent(data) {
      if (data.kind === "frame" && ctxRef.current) {
        renderFrame(data);
        setFrameCount((c) => c + 1);
        setLastFrameTime(new Date());
      } else if (data.kind === "status") {
        if (data.status === "vnc_required") {
          setStatus("vnc_required");
          setError("Headless device — open a local VNC session");
        }
      }
    }

    function renderFrame(data) {
      if (!data.jpeg_b64 || !ctxRef.current) return;

      // Decode base64 JPEG and render to canvas.
      const img = new Image();
      img.onload = () => {
        if (!ctxRef.current) return;
        const canvas = canvasRef.current;
        canvas.width = data.width || img.width;
        canvas.height = data.height || img.height;
        ctxRef.current.drawImage(img, 0, 0, canvas.width, canvas.height);
      };
      img.src = "data:image/jpeg;base64," + data.jpeg_b64;
    }

    return () => {
      cancelled = true;
      if (streamRef.current) {
        streamRef.current.close();
      }
      if (sessionIDRef.current) {
        api
          .stopSession(token, deviceID, { session_id: sessionIDRef.current })
          .catch(() => {});
      }
    };
  }, [deviceID, token, onUnauthorized]);

  const stopSession = useCallback(async () => {
    setStatus("stopped");
    if (streamRef.current) {
      streamRef.current.close();
    }
    if (sessionIDRef.current) {
      try {
        await api.stopSession(token, deviceID, {
          session_id: sessionIDRef.current,
        });
      } catch (e) {
        if (e.unauthorized) onUnauthorized();
      }
    }
    onClose();
  }, [token, deviceID, onClose, onUnauthorized]);

  const statusLabel = {
    connecting: "Connecting…",
    streaming: "Streaming",
    disconnected: "Disconnected",
    stopped: "Stopped",
    vnc_required: "VNC Required",
  };

  return (
    <div className="session-viewer">
      <div className="session-header">
        <div className="session-info">
          <h2>
            <Icon name="monitor" /> Remote Session
          </h2>
          <p className="session-device">{deviceName || deviceID}</p>
        </div>
        <div className="session-controls">
          <span
            className={
              "session-status " + (status === "streaming" ? "live" : "")
            }
          >
            {statusLabel[status] || status}
          </span>
          {connected && (
            <span className="session-frames">
              {frameCount} frames
              {lastFrameTime && ` (${timeAgo(lastFrameTime)})`}
            </span>
          )}
          <Button variant="ghost" onClick={stopSession}>
            <Icon name="power" /> End Session
          </Button>
        </div>
      </div>

      {error && (
        <Banner variant="error" onClose={() => setError(null)}>
          {error}
        </Banner>
      )}

      <div className="session-canvas">
        {status === "connecting" ? (
          <div className="canvas-placeholder">
            <p>Waiting for stream…</p>
          </div>
        ) : (
          <canvas
            ref={canvasRef}
            className="session-screen"
            width={1024}
            height={768}
          />
        )}
      </div>

      <div className="session-footer">
        <p className="session-note">
          Screen capture with Windows input support. Input events require the
          agent's SendInput driver (Windows SendInput API).
        </p>
      </div>
    </div>
  );
}

function timeAgo(date) {
  const diff = Date.now() - date.getTime();
  if (diff < 1000) return "just now";
  if (diff < 60000) return Math.floor(diff / 1000) + "s ago";
  return Math.floor(diff / 60000) + "m ago";
}
