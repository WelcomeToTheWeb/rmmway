// Kit empty state: the existing `.empty` block with an optional muted body
// line, an icon, and extra children (e.g. an action button).
// Icon accepts an SVG element or an emoji string.
export default function EmptyState({ title, body, icon, children }) {
  return (
    <div className="empty">
      {icon && <div className="empty-icon">{icon}</div>}
      {title}
      {body && <p className="muted">{body}</p>}
      {children}
    </div>
  );
}
