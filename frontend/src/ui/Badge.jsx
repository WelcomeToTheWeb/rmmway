// Kit badge family (existing classes):
//   Badge      — red count chip (`.badge`), e.g. open-alert counts
//   StatusPill — status pill (`.pill .pill-<status>`)
//   Score      — anomaly-score chip (`.score .hot/.warm/.cool`), σ display
export function fmtNum(n) {
	if (n === null || n === undefined) return "—";
	return (Math.round(n * 100) / 100).toString();
}

export function Badge({ children, className = "" }) {
	return (
		<span className={"badge" + (className ? " " + className : "")}>
			{children}
		</span>
	);
}

export function StatusPill({ status, children }) {
	return <span className={"pill pill-" + status}>{children ?? status}</span>;
}

// σ score → tone: ≥10σ hot, ≥5σ warm, else cool (baseline z-score bands).
export function scoreTone(score) {
	return score >= 10 ? "hot" : score >= 5 ? "warm" : "cool";
}

export function Score({ score }) {
	return (
		<span className={"score " + scoreTone(score)} title="z-score vs baseline">
			{fmtNum(score)}σ
		</span>
	);
}
