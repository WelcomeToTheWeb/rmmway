// Gap #4: patch management panel — query, approve, apply patches.
// Renders inside the expanded device row (DeviceDetail.jsx composes it).
import { useCallback, useState } from "react";
import { api } from "../../api.js";

export default function DevicePatches({ token, device, onUnauthorized }) {
  const [querying, setQuerying] = useState(false);
  const [applying, setApplying] = useState(false);
  const [patches, setPatches] = useState([]);
  const [selected, setSelected] = useState(new Set());
  const [message, setMessage] = useState(null);
  const [error, setError] = useState(null);

  const queryPatches = useCallback(async () => {
    setQuerying(true);
    setError(null);
    setMessage(null);
    try {
      // Trigger patch query via command dispatch
      const res = await api.dispatch(token, device.id, {
        action: "patch_query",
      });
      setMessage(
        "Patch query dispatched to agent. Results will appear after next heartbeat.",
      );
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setQuerying(false);
    }
  }, [token, device.id, onUnauthorized]);

  const togglePatch = (id) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
  };

  const applyPatches = useCallback(async () => {
    if (selected.size === 0) {
      setError("Select at least one patch to apply.");
      return;
    }
    setApplying(true);
    setError(null);
    setMessage(null);
    try {
      // Encode patch IDs as base64 JSON array
      const idsJson = JSON.stringify([...selected]);
      const idsB64 = btoa(idsJson);
      await api.dispatch(token, device.id, {
        action: "patch_apply",
        script: idsB64,
      });
      setMessage(`Patch apply dispatched for ${selected.size} patch(es).`);
      setSelected(new Set());
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setApplying(false);
    }
  }, [token, device.id, selected, onUnauthorized]);

  return (
    <div className="inventory-section patches-panel">
      <h3 className="inventory-section-title">Patch Management</h3>
      <div className="patches-controls">
        <button
          className="btn btn-small"
          onClick={queryPatches}
          disabled={querying}
        >
          {querying ? "Querying…" : "Query Available Patches"}
        </button>
        <button
          className="btn btn-small btn-primary"
          onClick={applyPatches}
          disabled={applying || selected.size === 0}
        >
          {applying ? "Applying…" : `Apply Selected (${selected.size})`}
        </button>
      </div>
      {message && <p className="patches-message">{message}</p>}
      {error && <p className="inventory-error">{error}</p>}
      <p className="patches-hint">
        Querying dispatches a command to the agent. Patch results are reported
        via heartbeat and stored server-side. Apply is async — check device
        commands for progress.
      </p>
    </div>
  );
}
