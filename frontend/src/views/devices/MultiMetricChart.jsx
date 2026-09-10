// Multi-metric dashboard: stacked mini-charts for CPU, Memory, Disk, Network.
// Provides at-a-glance health across all key dimensions simultaneously.
import { useCallback, useEffect, useState } from "react";
import { api } from "../../api.js";
import TimeSeriesChart from "../../ui/TimeSeriesChart.jsx";

const METRIC_DEFS = [
  { name: "cpu.utilization_percent", label: "CPU Utilization", source: "" },
  { name: "mem.used_percent", label: "Memory Used", source: "" },
  { name: "disk.used_percent", label: "Disk Used", source: "" },
  { name: "net.bytes_received", label: "Network In", source: "" },
];

const RANGES = ["1h", "6h", "24h", "7d"];

export default function MultiMetricChart({ token, device, onUnauthorized }) {
  const [range, setRange] = useState("6h");
  const [series, setSeries] = useState({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  const unauthorized = useCallback(
    (e) => e && e.unauthorized && onUnauthorized(),
    [onUnauthorized],
  );

  const loadAll = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const results = {};
      await Promise.all(
        METRIC_DEFS.map(async (def) => {
          try {
            const res = await api.metricsSeries(
              token,
              device.id,
              def.name,
              def.source,
              range,
            );
            results[def.name] = res;
            return res;
          } catch (e) {
            if (e.unauthorized) {
              throw e;
            }
            results[def.name] = null;
            return null;
          }
        }),
      );
      setSeries(results);
    } catch (e) {
      if (!unauthorized(e)) {
        setError("Failed to load metrics: " + e.message);
      }
    } finally {
      setLoading(false);
    }
  }, [token, device.id, range, unauthorized]);

  useEffect(() => {
    loadAll();
    const id = setInterval(loadAll, 60000);
    return () => clearInterval(id);
  }, [loadAll]);

  if (loading && Object.keys(series).length === 0) {
    return (
      <div className="multi-metric">
        <p className="muted">Loading metrics…</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="multi-metric">
        <div className="banner err">{error}</div>
      </div>
    );
  }

  return (
    <div className="multi-metric">
      <div className="multi-metric-header">
        <h3>Multi-Metric Overview</h3>
        <select
          className="search"
          value={range}
          onChange={(e) => setRange(e.target.value)}
          title="Time window"
        >
          {RANGES.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
      </div>

      <div className="multi-metric-grid">
        {METRIC_DEFS.map((def) => {
          const data = series[def.name];
          return (
            <div key={def.name} className="multi-metric-card">
              <h4>{def.label}</h4>
              {data && data.points && data.points.length > 0 ? (
                <TimeSeriesChart data={data} humanName={def.label} />
              ) : (
                <p className="muted">No data available</p>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
