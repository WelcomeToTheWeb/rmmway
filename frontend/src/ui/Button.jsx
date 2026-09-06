// Kit button: renders the existing `.btn` classes (primary/ghost/danger
// variants) plus a busy state (disabled + spinner).
export default function Button({
	variant,
	busy,
	className = "",
	disabled,
	children,
	...rest
}) {
	const cls = ["btn", variant, busy ? "busy" : "", className]
		.filter(Boolean)
		.join(" ");
	return (
		<button type="button" className={cls} disabled={disabled || busy} {...rest}>
			{busy && <span className="spinner" aria-hidden="true" />}
			{children}
		</button>
	);
}
