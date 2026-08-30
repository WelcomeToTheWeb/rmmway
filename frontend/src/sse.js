// Live event stream (W6-2 / B-1): a thin EventSource wrapper around the
// server's SSE route. The operator JWT rides ?token= because the browser
// EventSource API cannot set an Authorization header (the server's stream
// route accepts the JWT in either form).
//
// Each message is one journaled event envelope:
//   { id, version, source, category, type, device_id?, at, event }
// where `event` is the full bus event (its `data` field carries the
// action-specific detail, e.g. { action: "offline", device_id, reason }).
//
// The stream auto-reconnects with exponential backoff; the browser resumes
// from Last-Event-ID so a brief blip does not drop events.

// openEventStream subscribes to the operator event stream.
//   token    — the operator JWT (empty when not signed in; the route 401s).
//   onEvent  — called with each parsed envelope.
//   onState  — optional "open" | "reconnecting" transitions (UI indicator).
// Returns a cleanup function that closes the stream and stops reconnecting.
export function openEventStream({ token, onEvent, onState } = {}) {
  if (typeof EventSource === "undefined") return () => {};
  let es = null;
  let retryTimer = null;
  let retryMs = 1000;
  let closed = false;

  const connect = () => {
    if (closed) return;
    const q = new URLSearchParams();
    if (token) q.set("token", token);
    es = new EventSource("/api/events/stream?" + q.toString());
    es.onopen = () => {
      retryMs = 1000;
      if (onState) onState("open");
    };
    es.onerror = () => {
      if (closed) return;
      if (es) es.close();
      if (onState) onState("reconnecting");
      retryTimer = setTimeout(connect, retryMs);
      retryMs = Math.min(retryMs * 2, 30000);
    };
    es.onmessage = (e) => {
      if (!e.data) return;
      try {
        if (onEvent) onEvent(JSON.parse(e.data));
      } catch {
        /* non-JSON frame (e.g. a keepalive that slipped through) */
      }
    };
  };

  connect();
  return () => {
    closed = true;
    if (retryTimer) clearTimeout(retryTimer);
    if (es) es.close();
  };
}

// actionOf pulls the lifecycle action ("created" | "online" | "offline" |
// "fired" | "updated" | "resolved" | "notify" | ...) out of an envelope.
// The bus event is carried in `envelope.event`; the action rides either the
// top level or a nested `data` field depending on the producer, so check both.
export function actionOf(envelope) {
  if (!envelope || !envelope.event) return null;
  const ev = envelope.event;
  if (typeof ev !== "object") return null;
  return ev.action || (ev.data && ev.data.action) || null;
}
