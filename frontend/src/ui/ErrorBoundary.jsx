// Error boundary: catches render-time exceptions in the main content area
// so a bug in one view never leaves the operator staring at a blank page.
// The panel is kit-styled (.empty, tinted with the error banner tokens);
// the full trace goes to the console, the panel shows a one-line summary.
// App mounts one boundary per route (key={route}) so a caught error cannot
// block navigation to other views.
import { Component } from "react";
import { Icon } from "./Icon.jsx";

export default class ErrorBoundary extends Component {
	state = { error: null };

	static getDerivedStateFromError(error) {
		return { error };
	}

	componentDidCatch(error, info) {
		console.error("RMMWay view error:", error, info && info.componentStack);
	}

	render() {
		const { error } = this.state;
		if (!error) return this.props.children;
		const message =
			(error && (error.message || String(error))) || "Unknown error";
		return (
			<div className="error-fallback" role="alert">
				<div className="empty">
					<h2>Something went wrong</h2>
					<p className="muted">
						This view hit an unexpected error. Your data is safe — reload to pick up
						where you left off.
					</p>
					<pre>{message}</pre>
					<button
						type="button"
						className="btn primary"
						onClick={() => window.location.reload()}
					>
						<Icon name="refresh" />
						Reload
					</button>
				</div>
			</div>
		);
	}
}
