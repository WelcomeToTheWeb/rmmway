// Kit code block: monospace `<pre>` with a copy button (`.codeblock`
// styles). Copy degrades silently when the clipboard API is unavailable
// (non-secure context, headless jsdom).
import { useState } from "react";

export default function CodeBlock({ code, className = "" }) {
	const [copied, setCopied] = useState(false);
	const copy = async () => {
		try {
			await navigator.clipboard.writeText(code);
			setCopied(true);
			setTimeout(() => setCopied(false), 1500);
		} catch {
			/* clipboard unavailable — no-op */
		}
	};
	return (
		<div className={"codeblock" + (className ? " " + className : "")}>
			<pre>
				<code>{code}</code>
			</pre>
			<button
				type="button"
				className={"codeblock-copy" + (copied ? " copied" : "")}
				onClick={copy}
			>
				{copied ? "copied" : "copy"}
			</button>
		</div>
	);
}
