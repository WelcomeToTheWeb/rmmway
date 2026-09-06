// Kit banner: the existing `.banner` classes (err/ok/info tones).
export default function Banner({ tone = "info", className = "", children }) {
	return (
		<div className={"banner " + tone + (className ? " " + className : "")}>
			{children}
		</div>
	);
}
