// Gap #4: deep inventory panel — hardware, installed software, services.
// Renders inside the expanded device row (DeviceDetail.jsx composes it).
import { useCallback, useEffect, useState } from "react";
import { api } from "../../api.js";

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

function HardwareSection({ hardware }) {
  if (!hardware) {
    return (
      <div className="inventory-section">
        <h3 className="inventory-section-title">{SECTION_TITLE.hardware}</h3>
        <p className="inventory-empty">No hardware inventory collected yet.</p>
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
            <td className="inventory-value">{cpuCores} / {cpuLogical}</td>
          </tr>
          <tr>
            <td className="inventory-key">Memory</td>
            <td className="inventory-value">{ram}</td>
          </tr>
          <tr>
            <td className="inventory-key">OS</td>
            <td className="inventory-value">{os} {osVersion}</td>
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

function SoftwareSection({ software }) {
  if (!software || software.length === 0) {
    return (
      <div className="inventory-section">
        <h3 className="inventory-section-title">{SECTION_TITLE.software}</h3>
        <p className="inventory-empty">No software inventory collected yet.</p>
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

  useEffect(() => {
    load();
    const id = setInterval(load, 30000);
    return () => clearInterval(id);
  }, [load]);

  if (loading && !inventory) {
    return (
      <div className="inventory-panel">
        <p className="inventory-loading">Loading inventory…</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="inventory-panel">
        <p className="inventory-error">Error loading inventory: {error}</p>
        <button onClick={load} className="btn btn-small">Retry</button>
      </div>
    );
  }

  return (
    <div className="inventory-panel">
      <HardwareSection hardware={inventory?.hardware} />
      <SoftwareSection software={inventory?.software} />
      {inventory?.collected_at && (
        <p className="inventory-collected">
          Last collected: {new Date(inventory.collected_at).toLocaleString()}
        </p>
      )}
    </div>
  );
}