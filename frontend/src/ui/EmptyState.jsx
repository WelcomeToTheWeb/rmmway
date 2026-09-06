// Kit empty state: the existing `.empty` block with an optional muted body
// line and extra children (e.g. an action button).
export default function EmptyState({ title, body, children }) {
	return (
		<div className="empty">
			{title}
			{body && <p className="muted">{body}</p>}
			{children}
		</div>
	);
}
