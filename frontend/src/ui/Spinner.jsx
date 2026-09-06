// Kit spinner: the CSS `.spinner` (used standalone or inside busy buttons).
export default function Spinner({ label }) {
	return (
		<span className="spinner-wrap" role="status">
			<span className="spinner" aria-hidden="true" />
			{label && <span className="muted">{label}</span>}
		</span>
	);
}
