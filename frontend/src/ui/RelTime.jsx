// Kit relative timestamp: "Ns ago" style label with the absolute time in
// the native title (hover). Same buckets as the pre-kit alerts relTime.
export default function RelTime({ iso, title }) {
	if (!iso) return <span>—</span>;
	const d = new Date(iso);
	if (Number.isNaN(d.getTime())) return <span>{iso}</span>;
	const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
	let rel;
	if (s < 60) rel = `${Math.floor(s)}s ago`;
	else if (s < 3600) rel = `${Math.floor(s / 60)}m ago`;
	else if (s < 86400) rel = `${Math.floor(s / 3600)}h ago`;
	else rel = d.toLocaleString();
	return <span title={title || d.toLocaleString()}>{rel}</span>;
}
