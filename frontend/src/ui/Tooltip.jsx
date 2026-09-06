// Kit tooltip: pure-CSS hover/focus tooltip (`.ui-tip`), driven by
// data-tip. Keyboard focusable so keyboard users get the same info.
export default function Tooltip({ tip, children }) {
	return (
		<span className="ui-tip" data-tip={tip} tabIndex={0}>
			{children}
		</span>
	);
}
