// Kit table wrapper: the existing `.table-wrap` (`.table-wrap.sticky` adds a
// capped scroll area with a sticky header row). The <table> markup itself is
// provided by the caller so view-specific columns stay explicit.
export default function Table({ sticky, className = "devices", children }) {
	return (
		<div className={"table-wrap" + (sticky ? " sticky" : "")}>
			<table className={className}>{children}</table>
		</div>
	);
}
