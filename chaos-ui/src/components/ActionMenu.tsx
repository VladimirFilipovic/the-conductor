"use client";

import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

export interface MenuItem {
  key: string;
  label: string;
  hint?: string;
  danger?: boolean;
  onSelect: () => void;
}

const GAP = 4;

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
  const [pos, setPos] = useState<{ top: number; right: number } | null>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);

  // Portalled to <body> with fixed positioning: rows live inside overflow
  // containers (panel overflow-hidden, table overflow-x-auto) that would
  // otherwise clip an absolutely positioned popup to the row's box.
  useLayoutEffect(() => {
    if (!open || !trigger.current || !menu.current) return;
    const b = trigger.current.getBoundingClientRect();
    const h = menu.current.offsetHeight;
    const below = b.bottom + GAP + h <= window.innerHeight;
    setPos({
      top: below ? b.bottom + GAP : Math.max(GAP, b.top - GAP - h),
      right: window.innerWidth - b.right,
    });
  }, [open, items.length]);

  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      const t = e.target as Node;
      if (trigger.current?.contains(t) || menu.current?.contains(t)) return;
      setOpen(false);
    };
    const esc = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    // A fixed popup would drift away from its trigger on scroll; closing is
    // simpler than tracking every scrollable ancestor.
    const dismiss = () => setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", esc);
    window.addEventListener("scroll", dismiss, true);
    window.addEventListener("resize", dismiss);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", esc);
      window.removeEventListener("scroll", dismiss, true);
      window.removeEventListener("resize", dismiss);
    };
  }, [open]);

  const toggle = () => {
    setPos(null);
    setOpen(!open);
  };

  return (
    <div className={`inline-block ${className}`}>
      <button
        ref={trigger}
        className={`chip-btn ${open ? "chip-btn-active" : ""}`}
        onClick={toggle}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        {label}
      </button>
      {open &&
        createPortal(
          <div
            ref={menu}
            role="menu"
            className="menu"
            style={
              pos
                ? { top: pos.top, right: pos.right }
                : { top: 0, right: 0, visibility: "hidden" }
            }
          >
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
          </div>,
          document.body,
        )}
    </div>
  );
}
