// Kit tabs: the existing `.tabs`/`.tab` markup with role=tablist/tab
// semantics and arrow-key navigation (Left/Right/Home/End).
import { Badge } from "./Badge.jsx";

export default function Tabs({ tabs, value, onChange, className = "" }) {
	// tabs: [{ key, label, badge? }]
	const onKey = (e) => {
		const i = tabs.findIndex((t) => t.key === value);
		let next = null;
		if (e.key === "ArrowRight") next = (i + 1) % tabs.length;
		else if (e.key === "ArrowLeft") next = (i - 1 + tabs.length) % tabs.length;
		else if (e.key === "Home") next = 0;
		else if (e.key === "End") next = tabs.length - 1;
		if (next !== null) {
			e.preventDefault();
			onChange(tabs[next].key);
		}
	};
	return (
		<div className={"tabs" + (className ? " " + className : "")} role="tablist">
			{tabs.map((t) => (
				<button
					key={t.key || "all"}
					type="button"
					role="tab"
					aria-selected={t.key === value}
					className={"tab" + (t.key === value ? " active" : "")}
					onClick={() => onChange(t.key)}
					onKeyDown={onKey}
				>
					{t.label}
					{typeof t.badge === "number" && t.badge > 0 && <Badge>{t.badge}</Badge>}
				</button>
			))}
		</div>
	);
}
