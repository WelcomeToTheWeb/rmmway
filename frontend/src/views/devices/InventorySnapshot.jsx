// Inventory Snapshot — summary of device inventory (hardware, software count,
// last collection timestamp) with a "Collect now" button.
import { useCallback, useEffect, useState } from "react";
import { api } from "../../api.js";
import { Badge } from "../../ui/Badge.jsx";

function relTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export default function InventorySnapshot({
  token,
  device,
  onUnauthorized,
  onCollect,
}) {
  const [inventory, setInventory] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [collecting, setCollecting] = useState(false);

  const loadInventory = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await api.deviceInventory(token, device.id);
      setInventory(res);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setLoading(false);
    }
  }, [token, device.id, onUnauthorized]);

  useEffect(() => {
    loadInventory();
  }, [loadInventory]);

  const handleCollect = useCallback(async () => {
    if (collecting) return;
    setCollecting(true);
    try {
      await api.collectDeviceInventory(token, device.id);
      if (onCollect) onCollect();
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setCollecting(false);
    }
  }, [token, device.id, collecting, onUnauthorized, onCollect]);

  if (loading) {
    return (
      <div className="inventory-snapshot card">
        <h3>Inventory</h3>
        <p className="muted">Loading inventory…</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="inventory-snapshot card">
        <h3>Inventory</h3>
        <div className="banner err">{error}</div>
      </div>
    );
  }

  const hw = inventory ? inventory.hardware : null;
  const sw = inventory ? inventory.software : [];
  const collectedAt = inventory ? inventory.collected_at : null;

  return (
    <div className="inventory-snapshot card">
      <div className="inventory-snapshot-header">
        <h3>Inventory Snapshot</h3>
        <div className="row-actions">
          <button
            className="btn btn-small"
            onClick={handleCollect}
            disabled={collecting}
            title="Trigger inventory collection on this device"
          >
            {collecting ? "Collecting…" : "🔄 Collect Now"}
          </button>
          <a href="#/devices" className="muted tiny">
            view full inventory →
          </a>
        </div>
      </div>

      {collectedAt ? (
        <>
          <p className="muted tiny">Last collected: {relTime(collectedAt)}</p>
          <div className="inventory-snapshot-grid">
            <div className="inventory-stat">
              <span className="muted tiny">CPU</span>
              <span className="mono">{hw && hw.cpu ? hw.cpu : "—"}</span>
            </div>
            <div className="inventory-stat">
              <span className="muted tiny">Cores</span>
              <span className="mono">{hw && hw.cores ? hw.cores : "—"}</span>
            </div>
            <div className="inventory-stat">
              <span className="muted tiny">RAM</span>
              <span className="mono">
                {hw && hw.memory_bytes
                  ? `${Math.round(hw.memory_bytes / 1024 / 1024 / 1024)} GB`
                  : "—"}
              </span>
            </div>
            <div className="inventory-stat">
              <span className="muted tiny">Installed Packages</span>
              <Badge variant="info">{sw ? sw.length : 0}</Badge>
            </div>
          </div>
        </>
      ) : (
        <div className="empty">
          <p className="muted">No inventory collected yet.</p>
          <button
            className="btn btn-small"
            onClick={handleCollect}
            disabled={collecting}
          >
            {collecting ? "Collecting…" : "Collect Inventory"}
          </button>
        </div>
      )}
    </div>
  );
}
