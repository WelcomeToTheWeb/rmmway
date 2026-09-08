import { useCallback, useEffect, useState } from "react";
import { api } from "./api.js";
import {
	Button,
	Icon,
	Banner,
	Table,
	EmptyState,
	Modal,
	Field,
	RelTime,
} from "./ui/index.js";

// Clients is the MSP client/tenant view (gap #2, wave 1): the client
// registry with a per-client fleet rollup, and the per-client device list
// with one-click reassignment. A deployment always carries the seeded
// "Unassigned" default client — devices without an explicit assignment
// roll up under it, so a retrofit never strands a device.
const DEFAULT_CLIENT_ID = "unassigned";

// NewClientModal collects name + description for POST /api/clients.
function NewClientModal({ onClose, onCreate }) {
	const [name, setName] = useState("");
	const [description, setDescription] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState(null);

	async function submit() {
		setBusy(true);
		setError(null);
		try {
			await onCreate({ name, description });
		} catch (e) {
			if (e.unauthorized) return; // parent bounced to /login
			setError(e.message);
		} finally {
			setBusy(false);
		}
	}

	return (
		<Modal
			title="New client"
			onClose={onClose}
			footer={
				<>
					<Button variant="ghost" onClick={onClose} disabled={busy}>
						Cancel
					</Button>
					<Button onClick={submit} disabled={busy || !name.trim()}>
						{busy ? "creating…" : "Create client"}
					</Button>
				</>
			}
		>
			<Field
				label="Name"
				placeholder="e.g. Acme Corp"
				value={name}
				disabled={busy}
				onChange={(e) => setName(e.target.value)}
			/>
			<Field
				label="Description"
				as="textarea"
				rows={3}
				placeholder="account, contract, notes…"
				value={description}
				disabled={busy}
				onChange={(e) => setDescription(e.target.value)}
			/>
			{error && <Banner tone="err">{error}</Banner>}
		</Modal>
	);
}

// EditClientModal renames / re-describes an existing client.
function EditClientModal({ client, onClose, onSave }) {
	const [name, setName] = useState(client.name);
	const [description, setDescription] = useState(client.description || "");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState(null);

	async function submit() {
		setBusy(true);
		setError(null);
		try {
			await onSave({ name, description });
		} catch (e) {
			if (e.unauthorized) return;
			setError(e.message);
		} finally {
			setBusy(false);
		}
	}

	return (
		<Modal
			title={`Edit ${client.name}`}
			onClose={onClose}
			footer={
				<>
					<Button variant="ghost" onClick={onClose} disabled={busy}>
						Cancel
					</Button>
					<Button onClick={submit} disabled={busy || !name.trim()}>
						{busy ? "saving…" : "Save"}
					</Button>
				</>
			}
		>
			<Field
				label="Name"
				value={name}
				disabled={busy}
				onChange={(e) => setName(e.target.value)}
			/>
			<Field
				label="Description"
				as="textarea"
				rows={3}
				value={description}
				disabled={busy}
				onChange={(e) => setDescription(e.target.value)}
			/>
			{error && <Banner tone="err">{error}</Banner>}
		</Modal>
	);
}

// ClientDevices renders the selected client's device table with the
// per-row "move to client" select (PATCH /api/devices/{id}/client).
function ClientDevices({
	token,
	onUnauthorized,
	client,
	clients,
	onChanged,
	onBack,
}) {
	const [devices, setDevices] = useState(null);
	const [error, setError] = useState(null);
	const [moving, setMoving] = useState(null); // device id mid-move

	const load = useCallback(async () => {
		try {
			setDevices(await api.clientDevices(token, client.id));
			setError(null);
		} catch (e) {
			if (e.unauthorized) onUnauthorized();
			else setError(e.message);
		}
	}, [token, client.id, onUnauthorized]);

	useEffect(() => {
		load();
	}, [load]);

	async function move(deviceId, clientId) {
		setMoving(deviceId);
		setError(null);
		try {
			await api.assignDeviceClient(token, deviceId, clientId);
			await Promise.all([load(), onChanged()]);
		} catch (e) {
			if (e.unauthorized) onUnauthorized();
			else setError(e.message);
		} finally {
			setMoving(null);
		}
	}

	const sorted = [...clients].sort((a, b) => a.name.localeCompare(b.name));

	return (
		<div className="clients-detail">
			<div className="clients-detail-head">
				<Button variant="ghost" onClick={onBack} title="Back to clients">
					← clients
				</Button>
				<h3>
					{client.name}
					{client.id === DEFAULT_CLIENT_ID && (
						<span className="muted"> (default)</span>
					)}
				</h3>
			</div>
			{client.description && (
				<p className="muted clients-detail-desc">{client.description}</p>
			)}

			{error && <Banner tone="err">{error}</Banner>}

			{devices === null && !error ? (
				<EmptyState title="Loading devices…" />
			) : devices.length === 0 ? (
				<EmptyState
					title={
						client.id === DEFAULT_CLIENT_ID
							? "No unassigned devices."
							: "No devices assigned to this client yet."
					}
					body={
						client.id === DEFAULT_CLIENT_ID
							? "Devices land here automatically until you assign them a client — a healthy MSP has an empty list."
							: "Assign devices from the per-device view, or move them here from another client."
					}
				/>
			) : (
				<Table className="clients-devices">
					<thead>
						<tr>
							<th>Host</th>
							<th>OS</th>
							<th>Client</th>
							<th>Last seen</th>
						</tr>
					</thead>
					<tbody>
						{devices.map((d) => (
							<tr key={d.id}>
								<td>
									<div className="host">{d.hostname}</div>
									<div className="id mono">{d.id}</div>
								</td>
								<td className="muted">
									{d.os}
									{d.arch ? ` / ${d.arch}` : ""}
								</td>
								<td>
									<select
										className="search client-move"
										value={d.client_id || DEFAULT_CLIENT_ID}
										disabled={moving === d.id}
										onChange={(e) => move(d.id, e.target.value)}
										title={
											moving === d.id ? "moving…" : "move this device to another client"
										}
									>
										{sorted.map((c) => (
											<option key={c.id} value={c.id}>
												{c.name}
											</option>
										))}
									</select>
								</td>
								<td className="muted">
									<RelTime iso={d.last_seen} />
								</td>
							</tr>
						))}
					</tbody>
				</Table>
			)}
		</div>
	);
}

export default function Clients({ token, onUnauthorized }) {
	const [clients, setClients] = useState(null);
	const [error, setError] = useState(null);
	// unwired = the server answered 503 (in-memory mode: no client
	// registry) — show the explanatory banner instead of a raw HTTP 503.
	const [unwired, setUnwired] = useState(false);
	const [q, setQ] = useState("");
	const [selectedId, setSelectedId] = useState(null);
	const [modal, setModal] = useState(null); // null | "new" | client object

	const load = useCallback(async () => {
		try {
			setClients(await api.clients(token));
			setError(null);
			setUnwired(false);
		} catch (e) {
			if (e.unauthorized) onUnauthorized();
			else {
				setError(e.message);
				setUnwired(e.status === 503);
			}
		}
	}, [token, onUnauthorized]);

	useEffect(() => {
		load();
	}, [load]);

	async function createClient(body) {
		await api.createClient(token, {
			name: body.name.trim(),
			description: body.description.trim(),
		});
		setModal(null);
		await load();
	}

	async function saveClient(client, body) {
		await api.updateClient(token, client.id, {
			name: body.name.trim(),
			description: body.description.trim(),
		});
		setModal(null);
		await load();
	}

	const needle = q.trim().toLowerCase();
	const filtered = (clients || []).filter(
		(c) =>
			!needle ||
			c.name.toLowerCase().includes(needle) ||
			(c.description || "").toLowerCase().includes(needle),
	);
	const totalDevices = (clients || []).reduce(
		(n, c) => n + (c.device_count || 0),
		0,
	);
	const selected = (clients || []).find((c) => c.id === selectedId) || null;

	return (
		<section className="view clients">
			<div className="view-head">
				<div>
					<h2>Clients</h2>
					<p className="muted">
						{clients === null
							? "loading…"
							: `${clients.length} clients · ${totalDevices} devices`}
					</p>
				</div>
				<div className="view-actions">
					<input
						className="search"
						type="search"
						placeholder="filter: name, description"
						value={q}
						onChange={(e) => setQ(e.target.value)}
					/>
					<Button onClick={() => setModal("new")}>
						<Icon name="plus" size={14} /> new client
					</Button>
				</div>
			</div>

			{error && (
				<Banner tone="err">
					{unwired
						? "The client model needs a Postgres-backed server (in-memory mode has no client registry)."
						: error}
				</Banner>
			)}

			{selected ? (
				<ClientDevices
					token={token}
					onUnauthorized={onUnauthorized}
					client={selected}
					clients={clients || []}
					onChanged={load}
					onBack={() => setSelectedId(null)}
				/>
			) : clients === null && !error ? (
				<EmptyState title="Loading clients…" />
			) : filtered.length === 0 ? (
				<EmptyState
					title="No clients match."
					body={
						clients && clients.length === 1
							? "This deployment still only has the seeded Unassigned default client — create your first client to start grouping devices."
							: null
					}
				/>
			) : (
				<Table className="clients">
					<thead>
						<tr>
							<th>Client</th>
							<th>Description</th>
							<th>Devices</th>
							<th>Created</th>
							<th />
						</tr>
					</thead>
					<tbody>
						{filtered.map((c) => (
							<tr
								key={c.id}
								className="client-row"
								onClick={() => setSelectedId(c.id)}
								title="View this client's devices"
							>
								<td>
									<div className="host">
										{c.name}
										{c.id === DEFAULT_CLIENT_ID && (
											<span className="muted"> (default)</span>
										)}
									</div>
									<div className="id mono">{c.id}</div>
								</td>
								<td className="muted clients-desc">{c.description || "—"}</td>
								<td
									className="mono"
									title="devices assigned (unassigned devices count under the default client)"
								>
									{c.device_count || 0}
								</td>
								<td className="muted">
									<RelTime iso={c.created_at} />
								</td>
								<td className="row-actions" onClick={(e) => e.stopPropagation()}>
									<Button
										variant="ghost"
										onClick={() => setModal(c)}
										title="Rename / edit description"
									>
										edit
									</Button>
								</td>
							</tr>
						))}
					</tbody>
				</Table>
			)}

			{modal === "new" && (
				<NewClientModal onClose={() => setModal(null)} onCreate={createClient} />
			)}
			{modal && modal !== "new" && (
				<EditClientModal
					client={modal}
					onClose={() => setModal(null)}
					onSave={(body) => saveClient(modal, body)}
				/>
			)}
		</section>
	);
}
