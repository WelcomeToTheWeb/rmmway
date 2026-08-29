// Cmd-K command palette: type-to-search devices via Meilisearch, run
// actions (reboot / run script) from the keyboard, or "Go to device"
// to jump into the device list filtered to that host.
//
// Open with Ctrl+K / Cmd+K. Esc closes. Up/Down navigate, Enter selects.
import { useEffect, useRef, useState, useCallback } from "react";
import { useAuth } from "./auth.jsx";
import { api } from "./api.js";

// Static "action" rows that appear above the device hits when the query
// is empty or matches an action name.
const ACTIONS = [
  { kind: "action", id: "reboot", label: "Reboot selected device", hint: "Action" },
  { kind: "action", id: "run-script", label: "Run script on selected device", hint: "Action" },
  { kind: "action", id: "all-devices", label: "Go to all devices", hint: "Navigate" },
];

function b64(s) {
  // JS base64 of a UTF-8 string (script payloads are short).
  return btoa(unescape(encodeURIComponent(s)));
}

export default function Palette({ open, onClose, onGoToDevice, onGoToAll }) {
  const { token } = useAuth();
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState([]);
  const [loading, setLoading] = useState(false);
  const [selected, setSelected] = useState(0);
  const [busy, setBusy] = useState(null); // id of action being run
  const [error, setError] = useState(null);
  const inputRef = useRef(null);
  const listRef = useRef(null);
  const debounceRef = useRef(null);

  // Reset on open.
  useEffect(() => {
    if (open) {
      setQuery("");
      setHits([]);
      setSelected(0);
      setBusy(null);
      setError(null);
      // focus after the modal mounts
      requestAnimationFrame(() => inputRef.current?.focus());
    }
  }, [open]);

  // Debounced search against /api/search.
  useEffect(() => {
    if (!open) return;
    if (debounceRef.current) clearTimeout(debounceRef.current);
    if (!query.trim()) {
      setHits([]);
      setLoading(false);
      return;
    }
    setLoading(true);
    debounceRef.current = setTimeout(async () => {
      try {
        const res = await api.search(token, query.trim());
        // res = { hits: [...], estimatedTotalHits } or an array (some backends)
        const arr = Array.isArray(res) ? res : res.hits || [];
        setHits(arr.slice(0, 12));
      } catch (e) {
        setError(e.message || "search failed");
        setHits([]);
      } finally {
        setLoading(false);
      }
    }, 150);
    return () => debounceRef.current && clearTimeout(debounceRef.current);
  }, [query, open, token]);

  // Build the flat items list: action rows (when query is short/empty or
  // matches action name) followed by device hits.
  const items = [];
  if (!query.trim() || query.trim().length <= 3) {
    for (const a of ACTIONS) {
      if (!query.trim() || a.label.toLowerCase().includes(query.trim().toLowerCase())) {
        items.push(a);
      }
    }
  }
  for (const h of hits) {
    items.push({
      kind: "device",
      id: h.id || h.device_id,
      hostname: h.hostname || h.id || h.device_id,
      ip: (h.ip || h.interfaces || []).flatMap ? h.ip || [] : [],
      tags: h.tags || [],
      online: h.online,
      raw: h,
    });
  }

  // Reset selection when items change.
  useEffect(() => {
    if (selected >= items.length && items.length > 0) setSelected(items.length - 1);
    if (items.length === 0) setSelected(0);
  }, [items.length, selected]);

  // Scroll selected into view.
  useEffect(() => {
    const el = listRef.current?.children[selected];
    if (el) el.scrollIntoView({ block: "nearest" });
  }, [selected]);

  const runAction = useCallback(
    async (item) => {
      if (!item) return;
      setError(null);
      if (item.kind === "action") {
        if (item.id === "all-devices") {
          onClose();
          onGoToAll();
          return;
        }
        // Reboot / run-script need a target device: the selected row if it's
        // a device, otherwise the first device hit.
        const selectedRow = items[selected];
        const target =
          selectedRow && selectedRow.kind === "device"
            ? selectedRow
            : items.find((x) => x.kind === "device");
        if (!target) {
          setError("No device to target — type a device name first.");
          return;
        }
        setBusy(item.id);
        try {
          if (item.id === "reboot") {
            await api.dispatch(token, target.id, { action: "reboot" });
            setError(null);
            // keep open briefly so user sees success
            setTimeout(onClose, 400);
          } else if (item.id === "run-script") {
            const script = "#!/bin/sh\necho hello from RMMWay W2-2\necho $(date -u +%FT%TZ)";
            await api.dispatch(token, target.id, {
              action: "run_script",
              lang: "sh",
              script: b64(script),
            });
            setTimeout(onClose, 400);
          }
        } catch (e) {
          setError(e.message || "dispatch failed");
        } finally {
          setBusy(null);
        }
      } else if (item.kind === "device") {
        // Go to device.
        onClose();
        onGoToDevice(item.id, item.hostname);
      }
    },
    [items, selected, onClose, onGoToDevice, onGoToAll, token]
  );

  // Keyboard handling: attach when open.
  useEffect(() => {
    if (!open) return;
    const onKey = (e) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      } else if (e.key === "ArrowDown") {
        e.preventDefault();
        setSelected((s) => Math.min(s + 1, items.length - 1));
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        setSelected((s) => Math.max(s - 1, 0));
      } else if (e.key === "Enter") {
        e.preventDefault();
        runAction(items[selected]);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, items, selected, runAction, onClose]);

  if (!open) return null;

  return (
    <div className="palette-backdrop" onClick={onClose}>
      <div className="palette" onClick={(e) => e.stopPropagation()}>
        <div className="palette-header">
          <span className="palette-icon">⌘K</span>
          <input
            ref={inputRef}
            className="palette-input"
            type="text"
            placeholder="Search devices or type an action…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            disabled={busy !== null}
            autoFocus
          />
          <button className="palette-close" onClick={onClose} title="Close (Esc)">
            ×
          </button>
        </div>
        <div className="palette-body">
          {busy && <div className="palette-status busy">Running…</div>}
          {error && <div className="palette-status error">{error}</div>}
          {!busy && !error && (
            <ul ref={listRef} className="palette-list">
              {items.length === 0 && !loading && (
                <li className="palette-empty">No matches</li>
              )}
              {items.map((item, i) => (
                <li
                  key={item.kind + ":" + item.id}
                  className={
                    "palette-item" + (i === selected ? " selected" : "") + (item.kind === "action" ? " action" : "")
                  }
                  onMouseEnter={() => setSelected(i)}
                  onClick={() => runAction(item)}
                >
                  <span className="palette-item-label">{item.label || item.hostname}</span>
                  <span className="palette-item-hint">
                    {item.kind === "device" ? (
                      <>
                        {item.online ? "● online" : "○ offline"}
                        {item.id && <em> · {item.id}</em>}
                      </>
                    ) : (
                      item.hint
                    )}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
        <div className="palette-footer">
          <span>↑↓ navigate</span>
          <span>↵ select</span>
          <span>esc close</span>
          {loading && <span className="loading">searching…</span>}
        </div>
      </div>
    </div>
  );
}
