// Gap #4: deep inventory panel — hardware, installed software, services.
// Renders inside the expanded device row (DeviceDetail.jsx composes it).
import { useCallback, useEffect, useState } from "react";
import { api } from "../../api.js";
import { Skeleton } from "../../ui/index.js";
import DevicePatches from "./DevicePatches.jsx";

const SECTION_TITLE = {
  hardware: "Hardware",
  software: "Installed Software",
};

function fmtBytes(bytes) {
  if (!bytes) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let val = bytes;
  let unit = 0;
  while (val >= 1024 && unit < units.length - 1) {
    val /= 1024;
    unit++;
  }
  return `${val.toFixed(1)} ${units[unit]}`;
}

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

function HardwareSection({ hardware, collecting }) {
  if (!hardware) {
    return (
      <div className="inventory-section">
        <h3 className="inventory-section-title">{SECTION_TITLE.hardware}</h3>
        <p className="inventory-empty">No hardware inventory collected yet.</p>
        <button
          className="btn btn-small"
          onClick={collecting}
          disabled={collecting}
        >
          {collecting ? "Collecting…" : "🔄 Collect Inventory Now"}
        </button>
      </div>
    );
  }

  const cpuModel = hardware.cpu_model || "—";
  const cpuCores = hardware.cpu_cores || "—";
  const cpuLogical = hardware.cpu_logical || "—";
  const ram = fmtBytes(hardware.ram_total_bytes);
  const os = hardware.os_name || "—";
  const osVersion = hardware.os_version || "—";
  const hostname = hardware.hostname || "—";

  return (
    <div className="inventory-section">
      <h3 className="inventory-section-title">{SECTION_TITLE.hardware}</h3>
      <table className="inventory-table">
        <tbody>
          <tr>
            <td className="inventory-key">CPU</td>
            <td className="inventory-value">{cpuModel}</td>
          </tr>
          <tr>
            <td className="inventory-key">Cores (physical/logical)</td>
            <td className="inventory-value">
              {cpuCores} / {cpuLogical}
            </td>
          </tr>
          <tr>
            <td className="inventory-key">Memory</td>
            <td className="inventory-value">{ram}</td>
          </tr>
          <tr>
            <td className="inventory-key">OS</td>
            <td className="inventory-value">
              {os} {osVersion}
            </td>
          </tr>
          <tr>
            <td className="inventory-key">Hostname</td>
            <td className="inventory-value">{hostname}</td>
          </tr>
        </tbody>
      </table>
    </div>
  );
}

function SoftwareSection({ software, collecting }) {
  if (!software || software.length === 0) {
    return (
      <div className="inventory-section">
        <h3 className="inventory-section-title">{SECTION_TITLE.software}</h3>
        <p className="inventory-empty">No software inventory collected yet.</p>
        <button
          className="btn btn-small"
          onClick={collecting}
          disabled={collecting}
        >
          {collecting ? "Collecting…" : "🔄 Collect Inventory Now"}
        </button>
      </div>
    );
  }

  return (
    <div className="inventory-section">
      <h3 className="inventory-section-title">
        {SECTION_TITLE.software} ({software.length})
      </h3>
      <table className="inventory-table inventory-software">
        <thead>
          <tr>
            <th className="inventory-key">Name</th>
            <th className="inventory-value">Version</th>
            <th className="inventory-value">Source</th>
          </tr>
        </thead>
        <tbody>
          {software.map((sw) => (
            <tr key={sw.name}>
              <td className="inventory-key">{sw.name}</td>
              <td className="inventory-value">{sw.version || "—"}</td>
              <td className="inventory-value">{sw.source || "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// Deep inventory: hardware + installed software.
// Polls every 30s to pick up fresh collection results.
export default function DeviceInventory({ token, device, onUnauthorized }) {
  const [inventory, setInventory] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [collecting, setCollecting] = useState(false);

  const load = useCallback(async () => {
    try {
      const data = await api.deviceInventory(token, device.id);
      setInventory(data);
      setLoading(false);
      setError(null);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else {
        setError(e.message);
        setLoading(false);
      }
    }
  }, [token, device.id, onUnauthorized]);

  const collectInventory = useCallback(async () => {
    if (collecting) return;
    setCollecting(true);
    try {
      const res = await api.collectDeviceInventory(token, device.id);
      // Poll for updated inventory after collection is dispatched
      setTimeout(() => load(), 5000);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setCollecting(false);
    }
  }, [token, device.id, collecting, onUnauthorized, load]);

  useEffect(() => {
    load();
    const id = setInterval(load, 30000);
    return () => clearInterval(id);
  }, [load]);

  if (loading && !inventory) {
    return (
      <div className="inventory-panel">
        <Skeleton type="line" height="2rem" width="30%" />
        <Skeleton type="line" height="0.75rem" width="80%" />
        <Skeleton type="line" height="0.75rem" width="70%" />
        <Skeleton type="line" height="0.75rem" width="60%" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="inventory-panel">
        <p className="inventory-error">Error loading inventory: {error}</p>
        <button onClick={load} className="btn btn-small">
          Retry
        </button>
      </div>
    );
  }

  return (
    <div className="inventory-panel">
      {/* Patch Management moved to top (Phase 1.3) */}
      <DevicePatches
        token={token}
        device={device}
        onUnauthorized={onUnauthorized}
      />

      <HardwareSection
        hardware={inventory?.hardware}
        collecting={collectInventory}
      />
      <SoftwareSection
        software={inventory?.software}
        collecting={collectInventory}
      />
      {inventory?.collected_at && (
        <p className="inventory-collected">
          Last collected: {relTime(inventory.collected_at)}
        </p>
      )}
    </div>
  );
}
