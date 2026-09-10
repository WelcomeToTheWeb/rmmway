// Correlated metrics view: shows paired metrics together for cross-analysis.
// CPU+Load average, Memory+Swap, Network In+Out displayed as paired charts.
import { useCallback, useEffect, useState } from "react";
import { api } from "../../api.js";
import TimeSeriesChart from "../../ui/TimeSeriesChart.jsx";

const PAIRS = [
  {
    title: "CPU & Load",
    left: { name: "cpu.utilization_percent", label: "CPU Utilization" },
    right: { name: "system.load1", label: "Load Average (1m)" },
  },
  {
    title: "Memory & Swap",
    left: { name: "mem.used_percent", label: "Memory Used" },
    right: { name: "swap.used_percent", label: "Swap Used" },
  },
  {
    title: "Network In & Out",
    left: { name: "net.bytes_received", label: "Network In" },
    right: { name: "net.bytes_sent", label: "Network Out" },
  },
];

const RANGES = ["1h", "6h", "24h", "7d"];

function MetricPair({
  title,
  left,
  right,
  token,
  device,
  range,
  onUnauthorized,
}) {
  const [leftData, setLeftData] = useState(null);
  const [rightData, setRightData] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);

  const unauthorized = useCallback(
    (e) => e && e.unauthorized && onUnauthorized(),
    [onUnauthorized],
  );

  const loadData = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      await Promise.all([
        api
          .metricsSeries(token, device.id, left.name, "", range)
          .then((res) => setLeftData(res))
          .catch((e) => {
            if (!e.unauthorized) setLeftData(null);
          }),
        api
          .metricsSeries(token, device.id, right.name, "", range)
          .then((res) => setRightData(res))
          .catch((e) => {
            if (!e.unauthorized) setRightData(null);
          }),
      ]);
    } catch (e) {
      if (!unauthorized(e)) {
        setError("Failed to load metrics: " + e.message);
      }
    } finally {
      setLoading(false);
    }
  }, [token, device.id, left.name, right.name, range, unauthorized]);

  useEffect(() => {
    loadData();
    const id = setInterval(loadData, 60000);
    return () => clearInterval(id);
  }, [loadData]);

  if (loading && !leftData && !rightData) {
    return (
      <div className="metric-pair">
        <h4>{title}</h4>
        <p className="muted">Loading…</p>
      </div>
    );
  }

  if (error) {
    return (
      <div className="metric-pair">
        <h4>{title}</h4>
        <div className="banner err">{error}</div>
      </div>
    );
  }

  return (
    <div className="metric-pair">
      <h4>{title}</h4>
      <div className="metric-pair-charts">
        <div className="metric-pair-chart">
          {leftData && leftData.points && leftData.points.length > 0 ? (
            <TimeSeriesChart data={leftData} humanName={left.label} />
          ) : (
            <p className="muted">No data</p>
          )}
        </div>
        <div className="metric-pair-chart">
          {rightData && rightData.points && rightData.points.length > 0 ? (
            <TimeSeriesChart data={rightData} humanName={right.label} />
          ) : (
            <p className="muted">No data</p>
          )}
        </div>
      </div>
    </div>
  );
}

export default function CorrelatedMetrics({ token, device, onUnauthorized }) {
  const [range, setRange] = useState("6h");

  return (
    <div className="correlated-metrics">
      <div className="correlated-metrics-header">
        <h3>Correlated Metrics</h3>
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
      {PAIRS.map((pair) => (
        <MetricPair
          key={pair.title}
          {...pair}
          token={token}
          device={device}
          range={range}
          onUnauthorized={onUnauthorized}
        />
      ))}
    </div>
  );
}
