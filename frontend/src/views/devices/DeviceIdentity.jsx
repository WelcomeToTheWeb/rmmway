// Device Identity Card — the primary identification block for a device.
// Shows hostname, device ID, status, OS/arch, agent version, IPs, and client assignment.
import { useState } from "react";
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

export default function DeviceIdentity({ device, clients }) {
  const [showAllIPs, setShowAllIPs] = useState(false);
  const [copiedId, setCopiedId] = useState(false);

  // Find client name
  let clientName = "Unassigned";
  if (device.client_id && clients) {
    const client = clients.find((c) => c.id === device.client_id);
    if (client) clientName = client.name;
  }

  // Get primary IP
  let primaryIP = "—";
  if (device.interfaces && device.interfaces.length > 0) {
    for (const iface of device.interfaces) {
      for (const addr of iface.addrs || []) {
        if (
          addr.startsWith("192.") ||
          addr.startsWith("10.") ||
          addr.startsWith("172.")
        ) {
          primaryIP = addr;
          break;
        }
      }
      if (primaryIP !== "—") break;
    }
  }

  // Collect all IPs
  const allIPs = [];
  if (device.interfaces) {
    for (const iface of device.interfaces) {
      for (const addr of iface.addrs || []) {
        if (!allIPs.includes(addr)) allIPs.push(addr);
      }
    }
  }

  const copyDeviceId = () => {
    navigator.clipboard.writeText(device.id).then(() => {
      setCopiedId(true);
      setTimeout(() => setCopiedId(false), 2000);
    });
  };

  return (
    <div className="device-identity-card card">
      <div className="device-identity-header">
        <div>
          <h2 className="device-identity-hostname">
            {device.hostname || device.id}
          </h2>
          <div className="device-identity-id-row">
            <span className="mono muted" title="Device ID">
              {device.id}
            </span>
            <button
              className="btn btn-small ghost"
              onClick={copyDeviceId}
              title="Copy device ID"
            >
              {copiedId ? "✓ copied" : "⧉ copy"}
            </button>
          </div>
        </div>
        <div className="device-identity-status">
          <Badge variant={device.online ? "ok" : "offline"}>
            {device.online ? "online" : "offline"}
          </Badge>
          <span className="muted tiny">
            last seen {relTime(device.last_seen)}
          </span>
        </div>
      </div>

      <div className="device-identity-grid">
        <div className="device-identity-field">
          <span className="muted tiny">OS / Architecture</span>
          <span className="mono">
            {device.os}/{device.arch}
          </span>
        </div>
        <div className="device-identity-field">
          <span className="muted tiny">Agent Version</span>
          <span className="mono">{device.agent_version || "—"}</span>
        </div>
        <div className="device-identity-field">
          <span className="muted tiny">Primary IP</span>
          <span className="mono">{primaryIP}</span>
        </div>
        <div className="device-identity-field">
          <span className="muted tiny">Client</span>
          <span>{clientName}</span>
        </div>
      </div>

      {allIPs.length > 1 && (
        <div className="device-identity-ips">
          <button
            className="btn btn-small ghost"
            onClick={() => setShowAllIPs(!showAllIPs)}
          >
            {showAllIPs ? "Hide" : "Show"} all {allIPs.length} IPs
          </button>
          {showAllIPs && (
            <ul className="ip-list">
              {allIPs.map((ip) => (
                <li key={ip} className="mono">
                  {ip}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
