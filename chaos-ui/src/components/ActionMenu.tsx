"use client";

import { useEffect, useRef, useState } from "react";

export interface MenuItem {
  key: string;
  label: string;
  hint?: string;
  danger?: boolean;
  onSelect: () => void;
}

// One trigger per row instead of a strip of chips: the chaos verbs are rarely
// used and their explanations only make sense once you're looking at them.
export function ActionMenu({
  items,
  label = "⋯",
  className = "",
}: {
  items: MenuItem[];
  label?: string;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) setOpen(false);
    };
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
    };
  }, [open]);

  return (
    <div ref={ref} className={`relative inline-block ${className}`}>
      <button
        className={`chip-btn ${open ? "chip-btn-active" : ""}`}
        onClick={() => setOpen(!open)}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        {label}
      </button>
      {open && (
        <div role="menu" className="menu">
          {items.map((it) => (
            <button
              key={it.key}
              role="menuitem"
              className={`menu-item ${it.danger ? "menu-item-danger" : ""}`}
              onClick={() => {
                setOpen(false);
                it.onSelect();
              }}
            >
              <span>{it.label}</span>
              {it.hint && <span className="menu-hint">{it.hint}</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
