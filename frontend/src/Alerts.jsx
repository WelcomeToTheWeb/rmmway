import { useEffect, useState, useCallback } from "react";
import { api } from "./api.js";
import {
	Button,
	Icon,
	Tabs,
	Banner,
	Table,
	EmptyState,
	Score,
	StatusPill,
	RelTime,
	fmtNum,
} from "./ui/index.js";

// AlertRow renders one inbox entry with its triage actions.
function AlertRow({ a, busy, onAction, onUnauthorized }) {
	const [err, setErr] = useState(null);
	const act = async (status) => {
		setErr(null);
		try {
			await onAction(a.id, status);
		} catch (e) {
			if (e.unauthorized) onUnauthorized();
			else setErr(e.message);
		}
	};
	return (
		<tr className={"alert-" + a.status}>
			<td>
				<div className="host">{a.hostname || a.device_id}</div>
				<div className="id">{a.device_id}</div>
			</td>
			<td>
				<div className="host">{a.name}</div>
				<div className="id">
					{a.channel}
					{a.source ? ` · ${a.source}` : ""}
				</div>
			</td>
			<td className="mono">
				<Score score={a.score} />{" "}
				<span className="muted" title="observed vs baseline expected">
					{fmtNum(a.value)} ≈ {fmtNum(a.expected)}
				</span>
			</td>
			<td className="mono">
				<span
					className="events"
					title="anomaly passes folded into this alert (deduped)"
				>
					×{a.events}
				</span>
			</td>
			<td>
				<StatusPill status={a.status} />
				{err && <div className="row-err">{err}</div>}
			</td>
			<td className="muted" title="first seen">
				<RelTime iso={a.first_at} />
			</td>
			<td className="muted" title="last anomaly pass">
				<RelTime iso={a.last_at} />
			</td>
			<td>
				{busy ? (
					<span className="muted">…</span>
				) : a.status === "open" ? (
					<div className="row-actions">
						<Button
							variant="ghost"
							title="Mark acknowledged (stays in inbox)"
							onClick={() => act("acked")}
						>
							ack
						</Button>
						<Button
							title="Mark resolved (leaves inbox)"
							onClick={() => act("resolved")}
						>
							resolve
						</Button>
					</div>
				) : a.status === "acked" ? (
					<div className="row-actions">
						<Button
							title="Mark resolved (leaves inbox)"
							onClick={() => act("resolved")}
						>
							resolve
						</Button>
					</div>
				) : (
					<span className="muted">
						{a.resolved_at ? (
							<>
								resolved <RelTime iso={a.resolved_at} />
							</>
						) : (
							"—"
						)}
					</span>
				)}
			</td>
		</tr>
	);
}

const TABS = [
	{ key: "open", label: "Open" },
	{ key: "acked", label: "Acknowledged" },
	{ key: "resolved", label: "Resolved" },
	{ key: "", label: "All" },
];

// Alerts is the anomaly inbox: baseline-driven anomalies folded into one
// deduped alert per (device, metric, source). Open alerts auto-resolve when
// the series returns to baseline (or are acked/resolved manually).
// liveTick (B-1) is bumped by the parent on every alert-category event on
// the live stream, so a NEW alert appears in the open inbox immediately
// instead of on the next 10s poll.
export default function Alerts({ token, onUnauthorized, liveTick }) {
	const [alerts, setAlerts] = useState(null);
	const [counts, setCounts] = useState(null);
	const [error, setError] = useState(null);
	const [tab, setTab] = useState("open");
	const [q, setQ] = useState("");
	const [busy, setBusy] = useState(false);
	const [tick, setTick] = useState(0);

	const statusParam = tab === "All" ? "" : tab;

	const load = useCallback(async () => {
		try {
			const [list, c] = await Promise.all([
				api.alerts(token, { status: statusParam }),
				api.alertCounts(token),
			]);
			setAlerts(list);
			setCounts(c);
			setError(null);
		} catch (e) {
			if (e.unauthorized) onUnauthorized();
			else setError(e.message);
		}
	}, [token, statusParam, onUnauthorized]);

	useEffect(() => {
		load();
		const id = setInterval(load, 10000);
		return () => clearInterval(id);
	}, [load]);

	// B-1: an alert-category event arrives on the live stream (the parent
	// bumps liveTick); re-pull immediately so a new alert lands in the open
	// inbox the moment it fires, not on the next 10s poll. liveTick=0 is the
	// initial mount (load() already ran).
	useEffect(() => {
		if (liveTick && liveTick > 0) load();
	}, [liveTick, load]);

	// Keep "Ns ago" labels honest.
	useEffect(() => {
		const id = setInterval(() => setTick((t) => t + 1), 30000);
		return () => clearInterval(id);
	}, []);

	const act = useCallback(
		async (id, status) => {
			setBusy(true);
			try {
				await api.setAlertStatus(token, id, status);
				await load();
			} finally {
				setBusy(false);
			}
		},
		[token, load],
	);

	void tick;
	const needle = q.trim().toLowerCase();
	const filtered = (alerts || []).filter(
		(a) =>
			!needle ||
			(a.hostname || "").toLowerCase().includes(needle) ||
			a.device_id.toLowerCase().includes(needle) ||
			a.name.toLowerCase().includes(needle),
	);

	return (
		<section className="view">
			<div className="view-head">
				<div>
					<h2>Alerts</h2>
					<p className="muted">
						{alerts === null
							? "loading…"
							: counts
								? `${counts.open} open · ${counts.acked} acknowledged · ${counts.resolved} resolved`
								: "baseline-driven inbox — one alert per anomalous series"}
					</p>
				</div>
				<div className="view-actions">
					<input
						className="search"
						type="search"
						placeholder="filter: host, device, metric"
						value={q}
						onChange={(e) => setQ(e.target.value)}
					/>
					<Button onClick={load} title="Refresh now">
						<Icon name="refresh" size={14} /> refresh
					</Button>
				</div>
			</div>

			<Tabs
				tabs={TABS.map((t) =>
					t.key === "open" && counts && counts.open > 0
						? { ...t, badge: counts.open }
						: t,
				)}
				value={tab}
				onChange={setTab}
			/>

			{error && <Banner tone="err">{error}</Banner>}

			{alerts === null && !error ? (
				<EmptyState title="Loading alerts…" />
			) : filtered.length === 0 ? (
				<EmptyState
					title={
						statusParam === "open"
							? "No open alerts."
							: `No ${tab === "All" ? "" : tab + " "}alerts match.`
					}
					body={
						statusParam === "open"
							? "The baseline engine scores every metric hourly; when one drifts off its (day-of-week, hour-of-day) expectation it lands here as a single deduped alert — and auto-resolves once the metric returns to baseline."
							: null
					}
				/>
			) : (
				<Table className="devices alerts">
					<thead>
						<tr>
							<th>Host</th>
							<th>Metric</th>
							<th>Deviation</th>
							<th>Passes</th>
							<th>Status</th>
							<th>First</th>
							<th>Last</th>
							<th>Actions</th>
						</tr>
					</thead>
					<tbody>
						{filtered.map((a) => (
							<AlertRow
								key={a.id}
								a={a}
								busy={busy}
								onAction={act}
								onUnauthorized={onUnauthorized}
							/>
						))}
					</tbody>
				</Table>
			)}
		</section>
	);
}
