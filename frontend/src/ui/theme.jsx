// Theme state: dark default, light via [data-theme="light"] on
// documentElement, persisted to localStorage("rmmway-theme").
// index.html applies the saved theme before first paint; this module keeps
// React in sync and provides the top-bar toggle.
import { useCallback, useState } from "react";
import { Icon } from "./Icon.jsx";

const KEY = "rmmway-theme";

function currentTheme() {
	const t = document.documentElement.getAttribute("data-theme");
	return t === "light" || t === "dark" ? t : "dark";
}

export function applyTheme(theme) {
	document.documentElement.setAttribute("data-theme", theme);
	try {
		localStorage.setItem(KEY, theme);
	} catch {
		/* storage unavailable (private mode) — session-only theme */
	}
}

export function useTheme() {
	const [theme, setTheme] = useState(currentTheme);
	const toggle = useCallback(() => {
		setTheme((t) => {
			const next = t === "dark" ? "light" : "dark";
			applyTheme(next);
			return next;
		});
	}, []);
	return [theme, toggle];
}

// Top-bar toggle: shows the theme you'd switch TO.
export function ThemeToggle() {
	const [theme, toggle] = useTheme();
	const next = theme === "dark" ? "light" : "dark";
	return (
		<button
			type="button"
			className="theme-toggle"
			onClick={toggle}
			title={`Switch to ${next} theme`}
			aria-label={`Switch to ${next} theme`}
		>
			<Icon name={theme === "dark" ? "sun" : "moon"} />
		</button>
	);
}
