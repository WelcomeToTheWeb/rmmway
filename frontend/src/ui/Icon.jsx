// Minimal inline SVG icon set (stroke-based, inherits currentColor) for the
// component kit. Names are limited to what the UI actually renders.
const PATHS = {
	moon: <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />,
	sun: (
		<>
			<circle cx="12" cy="12" r="4" />
			<path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4m11.4-11.4 1.4-1.4" />
		</>
	),
	refresh: <path d="M21 12a9 9 0 1 1-2.64-6.36M21 3v6h-6" />,
	close: <path d="M18 6 6 18M6 6l12 12" />,
	copy: (
		<>
			<rect x="9" y="9" width="12" height="12" rx="2" />
			<path d="M5 15V5a2 2 0 0 1 2-2h10" />
		</>
	),
	check: <path d="M20 6 9 17l-5-5" />,
	plus: <path d="M12 5v14M5 12h14" />,
	chevronDown: <path d="m6 9 6 6 6-6" />,
	chevronRight: <path d="m9 18 6-6-6-6" />,
	search: (
		<>
			<circle cx="11" cy="11" r="7" />
			<path d="m21 21-4.3-4.3" />
		</>
	),
	bolt: <path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z" />,
};

export function Icon({ name, size = 16, ...rest }) {
	const node = PATHS[name];
	if (!node) return null;
	return (
		<svg
			width={size}
			height={size}
			viewBox="0 0 24 24"
			fill="none"
			stroke="currentColor"
			strokeWidth="2"
			strokeLinecap="round"
			strokeLinejoin="round"
			aria-hidden="true"
			focusable="false"
			{...rest}
		>
			{node}
		</svg>
	);
}
