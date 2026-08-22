import { useEffect, useState } from "react";

// W0-1 scaffold shell. The real app (auth, device list, Cmd-K palette)
// lands in W2-1 / W2-2. This view proves the frontend builds and runs and
// can reach the Go server through the Vite proxy.

function ServiceDot({ name, ok }) {
	return (
		<li style={{ display: "flex", alignItems: "center", gap: 8 }}>
			<span
				style={{
					width: 10,
					height: 10,
					borderRadius: "50%",
					background: ok ? "#22c55e" : "#ef4444",
					display: "inline-block",
				}}
			/>
			{ok ? name : `${name} (down)`}
		</li>
	);
}

export default function App() {
	const [health, setHealth] = useState(null);
	const [error, setError] = useState(null);

	useEffect(() => {
		let alive = true;
		const tick = async () => {
			try {
				const res = await fetch("/healthz");
				const data = await res.json();
				if (alive) {
					setHealth(data);
					setError(null);
				}
			} catch (e) {
				if (alive) setError(String(e));
			}
		};
		tick();
		const id = setInterval(tick, 5000);
		return () => {
			alive = false;
			clearInterval(id);
		};
	}, []);

	return (
		<main
			style={{
				fontFamily: "system-ui, sans-serif",
				maxWidth: 640,
				margin: "4rem auto",
				padding: "0 1rem",
			}}
		>
			<h1>RMMWay</h1>
			<p>W0-1 scaffold — local dev stack status</p>
			{error && (
				<p style={{ color: "#ef4444" }}>Server unreachable: {error}</p>
			)}
			{health && (
				<section>
					<p>
						Server {health.version} —{" "}
						{health.ok ? "all services healthy" : "degraded"}
					</p>
					<ul>
						{health.probes.map((p) => (
							<ServiceDot key={p.service} name={p.service} ok={p.ok} />
						))}
					</ul>
				</section>
			)}
		</main>
	);
}
