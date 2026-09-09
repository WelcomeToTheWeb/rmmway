import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "./api.js";

// D-7: the helpdesk / ticketing dashboard. Two tabs: Queue (all open and
// in-progress tickets, filterable by status/priority/queue) and My Tickets
// (tickets assigned to the current user). Each row links to the detail
// view with status transitions and notes.
//
// Status vocabulary (Ticket): open, in_progress, resolved, closed.
// Priority: low, medium, high, critical.
// Source: manual, heal, flow, alert.

function fmtAt(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? String(iso) : d.toLocaleString();
}

function fmtDuration(ms) {
  if (ms == null || Number.isNaN(ms) || ms < 0) return "—";
  const s = Math.round(ms / 1000);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m" + (s % 60) + "s";
  return Math.floor(s / 3600) + "h" + Math.floor((s % 3600) / 60) + "m";
}

function slaStatus(ticket) {
  const now = Date.now();
  if (ticket.first_response_due) {
    const due = new Date(ticket.first_response_due).getTime();
    if (due < now && !ticket.first_response_at) {
      return { field: "first_response", due: due - now, label: "response" };
    }
  }
  if (ticket.resolution_due) {
    const due = new Date(ticket.resolution_due).getTime();
    if (due < now && !ticket.resolved_at) {
      return { field: "resolution", due: due - now, label: "resolution" };
    }
  }
  return null;
}

function StatusPill({ status }) {
  return <span className={"pill tp ts-" + status}>{status}</span>;
}

function PriorityBadge({ priority }) {
  return <span className={"badge tb tb-" + priority}>{priority}</span>;
}

function SourceBadge({ source }) {
  if (!source || source === "manual") return null;
  return <span className={"badge tb tb-src-" + source}>{source}</span>;
}

function slaTimeRemaining(ticket) {
  const overdue = slaStatus(ticket);
  if (overdue) {
    return (
      <span className="tp-overdue">
        {overdue.label} {fmtDuration(-overdue.due)} ago
      </span>
    );
  }
  const now = Date.now();
  if (ticket.first_response_due && !ticket.first_response_at) {
    const due = new Date(ticket.first_response_due).getTime();
    if (due > now) {
      return (
        <span className="tp-due">
          response in {fmtDuration(due - now)}
        </span>
      );
    }
  }
  if (ticket.resolution_due && !ticket.resolved_at) {
    const due = new Date(ticket.resolution_due).getTime();
    if (due > now) {
      return (
        <span className="tp-due">
          resolution in {fmtDuration(due - now)}
        </span>
      );
    }
  }
  return null;
}

// Ticket detail view: status transitions, notes, assignment.
function TicketDetail({ ticket, onClose, onReload }) {
  const [noteText, setNoteText] = useState("");
  const [notes, setNotes] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  const loadNotes = useCallback(async () => {
    try {
      const res = await api.get(`/api/tickets/${ticket.id}/notes`);
      setNotes(res.data);
    } catch (e) {
      setError("Failed to load notes: " + e.message);
    }
  }, [ticket.id]);

  useEffect(() => {
    setLoading(true);
    loadNotes().finally(() => setLoading(false));
  }, [ticket.id, loadNotes]);

  const handleTransition = async (status) => {
    setBusy(true);
    try {
      const res = await api.post(`/api/tickets/${ticket.id}/transition`, { status });
      onReload(res.data);
    } catch (e) {
      setError("Transition failed: " + e.message);
    } finally {
      setBusy(false);
    }
  };

  const handleAddNote = async () => {
    if (!noteText.trim()) return;
    setBusy(true);
    try {
      await api.post(`/api/tickets/${ticket.id}/notes`, {
        content: noteText,
      });
      setNoteText("");
      loadNotes();
    } catch (e) {
      setError("Failed to add note: " + e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="tp-detail">
      <div className="tp-detail-head">
        <h3>
          {ticket.title}
          <span className="tp-id">{ticket.id}</span>
        </h3>
        <button className="btn ghost" onClick={onClose}>
          ✕ Close
        </button>
      </div>
      {error && <div className="banner err">{error}</div>}
      <div className="tp-detail-meta">
        <StatusPill status={ticket.status} />
        <PriorityBadge priority={ticket.priority} />
        <SourceBadge source={ticket.source} />
        {slaTimeRemaining(ticket)}
        {ticket.assigned_to && <span className="muted">assigned: {ticket.assigned_to}</span>}
        {ticket.device_id && <span className="muted">device: {ticket.device_id}</span>}
        <span className="muted">created: {fmtAt(ticket.created_at)}</span>
      </div>
      <div className="tp-detail-body">
        <p>{ticket.description}</p>
      </div>
      <div className="tp-transitions">
        <h4>Transitions</h4>
        {ticket.status !== "in_progress" && (
          <button
            className="btn small"
            disabled={busy}
            onClick={() => handleTransition("in_progress")}
          >
            Start work
          </button>
        )}
        {ticket.status !== "resolved" && (
          <button
            className="btn small success"
            disabled={busy}
            onClick={() => handleTransition("resolved")}
          >
            Mark resolved
          </button>
        )}
        {ticket.status !== "closed" && (
          <button
            className="btn small"
            disabled={busy}
            onClick={() => handleTransition("closed")}
          >
            Close ticket
          </button>
        )}
        {ticket.status !== "open" && (
          <button
            className="btn small warning"
            disabled={busy}
            onClick={() => handleTransition("open")}
          >
            Reopen
          </button>
        )}
      </div>
      <div className="tp-notes">
        <h4>Notes</h4>
        {loading ? (
          <p className="muted">Loading notes…</p>
        ) : notes.length === 0 ? (
          <p className="muted">No notes yet.</p>
        ) : (
          <ul className="tp-note-list">
            {notes.map((n) => (
              <li key={n.id} className="tp-note">
                <span className="tp-note-meta">
                  {n.author && <strong>{n.author}</strong>}
                  {n.is_internal && <span className="badge">internal</span>}
                  <span className="muted">{fmtAt(n.created_at)}</span>
                </span>
                <p>{n.content}</p>
              </li>
            ))}
          </ul>
        )}
        <div className="tp-note-add">
          <textarea
            value={noteText}
            onChange={(e) => setNoteText(e.target.value)}
            placeholder="Add a note…"
            rows={3}
          />
          <button
            className="btn small"
            disabled={busy || !noteText.trim()}
            onClick={handleAddNote}
          >
            Add note
          </button>
        </div>
      </div>
    </div>
  );
}

// Ticket row in queue / my-tickets list.
function TicketRow({ ticket, onClick }) {
  return (
    <tr className="tp-row" onClick={onClick}>
      <td className="tp-id-cell">
        <span className="tp-id">{ticket.id}</span>
        <PriorityBadge priority={ticket.priority} />
      </td>
      <td className="tp-title-cell">
        <strong>{ticket.title}</strong>
        <SourceBadge source={ticket.source} />
        {ticket.device_id && <span className="muted"> · {ticket.device_id}</span>}
      </td>
      <td className="tp-status-cell">
        <StatusPill status={ticket.status} />
      </td>
      <td className="tp-queue-cell">{ticket.queue}</td>
      <td className="tp-assignee-cell">
        {ticket.assigned_to || <span className="muted">unassigned</span>}
      </td>
      <td className="tp-sla-cell">{slaTimeRemaining(ticket)}</td>
      <td className="tp-age-cell muted">{fmtAt(ticket.created_at)}</td>
    </tr>
  );
}

export default function Tickets() {
  const [tab, setTab] = useState("queue");
  const [tickets, setTickets] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [filter, setFilter] = useState({
    status: "",
    priority: "",
    queue: "",
    assigned_to: "",
  });

  const loadTickets = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      if (tab === "queue") {
        params.set("status", "open,in_progress");
      }
      if (filter.status && filter.status !== "open,in_progress") {
        params.set("status", filter.status);
      }
      if (filter.priority) params.set("priority", filter.priority);
      if (filter.queue) params.set("queue", filter.queue);
      if (filter.assigned_to) params.set("assigned_to", filter.assigned_to);
      const res = await api.get(`/api/tickets?${params.toString()}`);
      setTickets(res.data);
    } catch (e) {
      setError("Failed to load tickets: " + e.message);
    } finally {
      setLoading(false);
    }
  }, [tab, filter]);

  useEffect(() => {
    loadTickets();
  }, [loadTickets]);

  const selectedTicket = useState(null);

  return (
    <div className="tickets-page">
      <h2>Tickets</h2>
      <div className="tp-tabs">
        <button
          className={tab === "queue" ? "btn active" : "btn ghost"}
          onClick={() => setTab("queue")}
        >
          Queue
        </button>
        <button
          className={tab === "mine" ? "btn active" : "btn ghost"}
          onClick={() => setTab("mine")}
        >
          My Tickets
        </button>
      </div>
      <div className="tp-filters">
        <select
          value={filter.status}
          onChange={(e) => setFilter({ ...filter, status: e.target.value })}
        >
          <option value="">All statuses</option>
          <option value="open">Open</option>
          <option value="in_progress">In Progress</option>
          <option value="resolved">Resolved</option>
          <option value="closed">Closed</option>
        </select>
        <select
          value={filter.priority}
          onChange={(e) => setFilter({ ...filter, priority: e.target.value })}
        >
          <option value="">All priorities</option>
          <option value="low">Low</option>
          <option value="medium">Medium</option>
          <option value="high">High</option>
          <option value="critical">Critical</option>
        </select>
        <input
          type="text"
          placeholder="Queue…"
          value={filter.queue}
          onChange={(e) => setFilter({ ...filter, queue: e.target.value })}
        />
        <input
          type="text"
          placeholder="Assigned to…"
          value={filter.assigned_to}
          onChange={(e) => setFilter({ ...filter, assigned_to: e.target.value })}
        />
      </div>
      {error && <div className="banner err">{error}</div>}
      {loading ? (
        <p className="muted">Loading tickets…</p>
      ) : tickets.length === 0 ? (
        <p className="muted">No tickets found.</p>
      ) : (
        <div className="table-wrap">
          <table className="tp-table">
            <thead>
              <tr>
                <th>ID / Priority</th>
                <th>Title</th>
                <th>Status</th>
                <th>Queue</th>
                <th>Assigned</th>
                <th>SLA</th>
                <th>Age</th>
              </tr>
            </thead>
            <tbody>
              {tickets.map((t) => (
                <TicketRow
                  key={t.id}
                  ticket={t}
                  onClick={() => selectedTicket[1](t)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {selectedTicket[0] && (
        <div className="tp-detail-overlay">
          <TicketDetail
            ticket={selectedTicket[0]}
            onClose={() => selectedTicket[1](null)}
            onReload={(updated) => selectedTicket[1](updated)}
          />
        </div>
      )}
    </div>
  );
}