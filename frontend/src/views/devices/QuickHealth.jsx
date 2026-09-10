// Quick Health Snapshot — last hour sparklines for CPU, Memory, Disk, Network.
// Lightweight version of the full Metrics tab for quick glanceability.
import { useCallback, useEffect, useState } from "react";
import { api } from "../../api.js";
import { Skeleton } from "../../ui/index.js";

// Simple SVG sparkline from data points
function Sparkline({ data, color = "#3a6ea5", height = 40 }) {
  if (!data || !data.length) {
    return (
      <svg width="120" height={height} className="muted">
        <text x="10" y={height / 2 + 4} fontSize="11">
          no data
        </text>
      </svg>
    );
  }

  const values = data.map((p) => p[1]);
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;
  const w = 120;

  const points = data
    .map((p, i) => {
      const x = (i / (data.length - 1)) * w;
      const y = height - ((p[1] - min) / range) * (height - 4) - 2;
      return `${x},${y}`;
    })
    .join(" ");

  return (
    <svg width={w} height={height}>
      <polyline
        points={points}
        fill="none"
        stroke={color}
        strokeWidth="2"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function MetricTile({ name, data, unit, color, loading }) {
  if (loading || !data) {
    return (
      <div className="metric-tile">
        <span className="muted">{name}</span>
        <div className="metric-sparkline-loading">
          <Skeleton type="line" height="0.5rem" width="60px" />
        </div>
      </div>
    );
  }

  const lastPoint = data[data.length - 1];
  const lastValue = lastPoint ? lastPoint[1] : null;

  return (
    <div className="metric-tile">
      <span className="muted">{name}</span>
      <div className="metric-value">
        <span className="metric-current mono">
          {lastValue === null ? "—" : `${lastValue.toFixed(1)}${unit}`}
        </span>
      </div>
      <Sparkline data={data} color={color} />
    </div>
  );
}

export default function QuickHealth({ token, device, onUnauthorized }) {
  const [cpuData, setCpuData] = useState(null);
  const [memData, setMemData] = useState(null);
  const [diskData, setDiskData] = useState(null);
  const [netData, setNetData] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  const fetchMetric = useCallback(
    async (name, source = "", setter) => {
      try {
        const res = await api.metricsSeries(
          token,
          device.id,
          name,
          source,
          "1h",
        );
        if (res && res.points) {
          setter(res.points);
        }
      } catch (e) {
        if (!e.unauthorized) {
          // Metric might not exist — that's ok for quick health
        }
      }
    },
    [token, device.id],
  );

  const loadAll = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      await Promise.all([
        fetchMetric("cpu.utilization_percent", "", setCpuData),
        fetchMetric("mem.used_percent", "", setMemData),
        fetchMetric("disk.used_percent", "", setDiskData),
        fetchMetric("net.bytes_received", "", setNetData),
      ]);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setError(e.message);
    } finally {
      setLoading(false);
    }
  }, [fetchMetric, onUnauthorized]);

  useEffect(() => {
    loadAll();
    const id = setInterval(loadAll, 60000);
    return () => clearInterval(id);
  }, [loadAll]);

  return (
    <div className="quick-health card">
      <div className="quick-health-header">
        <h3>Quick Health (last hour)</h3>
        <a href="#/devices" className="muted tiny">
          view full metrics →
        </a>
      </div>
      {error && <div className="banner err">{error}</div>}
      <div className="quick-health-grid">
        <MetricTile
          name="CPU"
          data={cpuData}
          unit="%"
          color="#e74c3c"
          loading={loading}
        />
        <MetricTile
          name="Memory"
          data={memData}
          unit="%"
          color="#3498db"
          loading={loading}
        />
        <MetricTile
          name="Disk"
          data={diskData}
          unit="%"
          color="#2ecc71"
          loading={loading}
        />
        <MetricTile
          name="Network In"
          data={netData}
          unit="B/s"
          color="#9b59b6"
          loading={loading}
        />
      </div>
    </div>
  );
}
