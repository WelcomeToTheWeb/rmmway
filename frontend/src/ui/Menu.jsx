// Menu: minimal dropdown — a toolbar button that opens an absolutely
// positioned panel of caller-supplied items. Zero-dependency, class-based
// (styles live in styles/kit.css: .menu / .menu-panel / .menu-item).
// Closes on outside click and Escape; items do NOT auto-close it, so a
// show/hide checklist can be edited with successive clicks.
import { useEffect, useRef, useState } from "react";

export default function Menu({ label, title, align = "right", children }) {
  const [open, setOpen] = useState(false);
  const ref = useRef(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e) => {
      if (ref.current && !ref.current.contains(e.target)) setOpen(false);
    };
    const onKey = (e) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className={"menu" + (align === "left" ? " menu-left" : "")} ref={ref}>
      <button
        className="btn menu-toggle"
        aria-expanded={open}
        aria-haspopup="true"
        title={title}
        onClick={() => setOpen((v) => !v)}
      >
        {label}
      </button>
      {open && <div className="menu-panel">{children}</div>}
    </div>
  );
}
