import { useEffect, useState, useCallback } from "react";
import { api } from "./api.js";
import {
  Button,
  Icon,
  Tabs,
  Banner,
  Table,
  EmptyState,
  StatusPill,
  RelTime,
} from "./ui/index.js";
import "./views/reports.css";

// Reports page: scheduled reports + on-demand generation.
export default function Reports({ token, onUnauthorized }) {
  const [tab, setTab] = useState("schedules");
  const [schedules, setSchedules] = useState([]);
  const [runs, setRuns] = useState([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState(null);

  const loadSchedules = useCallback(async () => {
    try {
      const data = await api.reportSchedules(token);
      setSchedules(data.schedules || []);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
  }, [token, onUnauthorized]);

  const loadRuns = useCallback(async () => {
    try {
      const data = await api.reportRuns(token);
      setRuns(data.runs || []);
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
  }, [token, onUnauthorized]);

  useEffect(() => {
    loadSchedules();
  }, [loadSchedules]);

  useEffect(() => {
    if (tab === "runs") loadRuns();
  }, [tab, loadRuns]);

  const createSchedule = async (name, reportType, schedule) => {
    setBusy(true);
    setErr(null);
    try {
      await api.createReportSchedule(token, {
        name,
        report_type: reportType,
        schedule,
        output_format: "csv",
      });
      loadSchedules();
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
    setBusy(false);
  };

  const deleteSchedule = async (id) => {
    setBusy(true);
    setErr(null);
    try {
      await api.deleteReportSchedule(token, id);
      loadSchedules();
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
    setBusy(false);
  };

  const generateReport = async (reportType, deviceID) => {
    setBusy(true);
    setErr(null);
    try {
      const body = { report_type: reportType, output_format: "csv" };
      if (deviceID) body.device_id = deviceID;
      await api.generateReport(token, body);
      loadRuns();
    } catch (e) {
      if (e.unauthorized) onUnauthorized();
      else setErr(e.message);
    }
    setBusy(false);
  };

  return (
    <div className="reports">
      <div className="page-header">
        <h1>Reports</h1>
        <p>Scheduled and on-demand fleet reports.</p>
      </div>

      {err && (
        <Banner variant="error" onClose={() => setErr(null)}>
          {err}
        </Banner>
      )}

      <Tabs
        tabs={[
          { label: "Schedules", value: "schedules" },
          { label: "Runs", value: "runs" },
          { label: "Generate", value: "generate" },
        ]}
        value={tab}
        onChange={setTab}
      />

      {tab === "schedules" && (
        <div className="schedules">
          <NewScheduleForm
            busy={busy}
            onCreate={createSchedule}
          />
          {schedules.length === 0 ? (
            <EmptyState
              icon="calendar"
              title="No report schedules"
              subtitle="Create a schedule to run reports automatically."
            />
          ) : (
            <Table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Type</th>
                  <th>Schedule</th>
                  <th>Format</th>
                  <th>Enabled</th>
                  <th>Created</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {schedules.map((s) => (
                  <tr key={s.id}>
                    <td className="host">{s.name}</td>
                    <td>{reportTypeName(s.report_type)}</td>
                    <td className="mono">{s.schedule}</td>
                    <td className="mono">{s.output_format}</td>
                    <td>
                      <StatusPill status={s.enabled ? "open" : "resolved"} />
                    </td>
                    <td className="muted">
                      <RelTime iso={s.created_at} />
                    </td>
                    <td>
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={busy}
                        onClick={() => deleteSchedule(s.id)}
                      >
                        <Icon name="trash" />
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </Table>
          )}
        </div>
      )}

      {tab === "runs" && (
        <div className="runs">
          <Table>
            <thead>
              <tr>
                <th>Report</th>
                <th>Type</th>
                <th>Triggered</th>
                <th>Status</th>
                <th>Started</th>
                <th>Finished</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr key={r.id}>
                  <td className="host">Run #{r.id}</td>
                  <td>{reportTypeName(r.report_type)}</td>
                  <td className="mono">{r.triggered_by}</td>
                  <td>
                    <StatusPill status={r.status} />
                  </td>
                  <td className="muted">
                    <RelTime iso={r.started_at} />
                  </td>
                  <td className="muted">
                    {r.finished_at ? (
                      <RelTime iso={r.finished_at} />
                    ) : (
                      "—"
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </Table>
        </div>
      )}

      {tab === "generate" && (
        <div className="generate">
          <p>Generate an on-demand report.</p>
          <div className="generate-buttons">
            <Button
              disabled={busy}
              onClick={() => generateReport("fleet_status")}
            >
              Fleet Status (CSV)
            </Button>
            <Button
              disabled={busy}
              onClick={() => generateReport("device")}
            >
              Device Report (CSV)
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}

function reportTypeName(type) {
  switch (type) {
    case "fleet_status":
      return "Fleet Status";
    case "device":
      return "Device Report";
    case "patch_compliance":
      return "Patch Compliance";
    case "license_compliance":
      return "License Compliance";
    case "uptime_sla":
      return "Uptime/SLA";
    default:
      return type;
  }
}

// NewScheduleForm collects inputs for a new report schedule.
function NewScheduleForm({ busy, onCreate }) {
  const [name, setName] = useState("");
  const [type, setType] = useState("fleet_status");
  const [interval, setInterval] = useState("24h");
  const [localErr, setLocalErr] = useState(null);

  const submit = (e) => {
    e.preventDefault();
    setLocalErr(null);
    if (!name) {
      setLocalErr("Name is required");
      return;
    }
    onCreate(name, type, interval);
    setName("");
  };

  return (
    <form className="new-schedule-form" onSubmit={submit}>
      <h3>New Schedule</h3>
      {localErr && (
        <Banner variant="error" onClose={() => setLocalErr(null)}>
          {localErr}
        </Banner>
      )}
      <div className="form-row">
        <label>Name</label>
        <input
          type="text"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. Daily fleet report"
        />
      </div>
      <div className="form-row">
        <label>Type</label>
        <select value={type} onChange={(e) => setType(e.target.value)}>
          <option value="fleet_status">Fleet Status</option>
          <option value="device">Device Report</option>
          <option value="patch_compliance">Patch Compliance</option>
          <option value="uptime_sla">Uptime/SLA</option>
        </select>
      </div>
      <div className="form-row">
        <label>Interval</label>
        <select value={interval} onChange={(e) => setInterval(e.target.value)}>
          <option value="1h">Every hour</option>
          <option value="6h">Every 6 hours</option>
          <option value="24h">Daily</option>
          <option value="168h">Weekly</option>
        </select>
      </div>
      <Button type="submit" disabled={busy}>
        {busy ? "Creating…" : "Create Schedule"}
      </Button>
    </form>
  );
}
