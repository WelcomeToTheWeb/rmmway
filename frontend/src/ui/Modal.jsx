// Kit modal: the existing `.modal-backdrop`/`.modal` markup with Esc +
// backdrop-click close and a basic focus trap (Tab cycles inside the dialog,
// focus returns to the opener on close).
import { useEffect, useRef } from "react";
import IconButton from "./IconButton.jsx";

const FOCUSABLE =
	'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';

export default function Modal({ title, onClose, children, footer }) {
	const ref = useRef(null);

	useEffect(() => {
		const el = ref.current;
		if (!el) return;
		const prev = document.activeElement;
		const focusables = () => [...el.querySelectorAll(FOCUSABLE)];
		const first = focusables();
		if (first.length) first[0].focus();
		else el.focus();

		const onKey = (e) => {
			if (e.key === "Escape") {
				if (onClose) onClose();
				return;
			}
			if (e.key !== "Tab") return;
			const f = focusables();
			if (!f.length) return;
			const firstEl = f[0];
			const lastEl = f[f.length - 1];
			if (e.shiftKey && document.activeElement === firstEl) {
				e.preventDefault();
				lastEl.focus();
			} else if (!e.shiftKey && document.activeElement === lastEl) {
				e.preventDefault();
				firstEl.focus();
			}
		};
		document.addEventListener("keydown", onKey);
		return () => {
			document.removeEventListener("keydown", onKey);
			if (prev && typeof prev.focus === "function") prev.focus();
		};
	}, [onClose]);

	return (
		<div
			className="modal-backdrop"
			onMouseDown={(e) => {
				if (e.target === e.currentTarget && onClose) onClose();
			}}
		>
			<div
				className="modal"
				role="dialog"
				aria-modal="true"
				aria-label={typeof title === "string" ? title : undefined}
				tabIndex={-1}
				ref={ref}
			>
				<div className="modal-head">
					<h3>{title}</h3>
					<IconButton name="close" label="Close" onClick={onClose} />
				</div>
				{children}
				{footer && <div className="modal-foot">{footer}</div>}
			</div>
		</div>
	);
}
