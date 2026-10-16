import { useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";

const WIDTH = 300;
const MARGIN = 12;

/**
 * A "?" that explains something on hover or keyboard focus. The text is rendered into <body>
 * at a fixed position clamped to the window, so a card with overflow: hidden (the graph card)
 * can never cut it off, whatever the screen width.
 */
export default function Hint({ text }: { text: string }) {
  const anchor = useRef<HTMLSpanElement>(null);
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ left: number; top: number } | null>(null);

  useLayoutEffect(() => {
    if (!open || !anchor.current) return;
    const r = anchor.current.getBoundingClientRect();
    const width = Math.min(WIDTH, window.innerWidth - 2 * MARGIN);
    const left = Math.max(MARGIN, Math.min(r.left + r.width / 2 - width / 2, window.innerWidth - width - MARGIN));
    setPos({ left, top: r.bottom + 8 });
  }, [open]);

  return (
    <span
      ref={anchor}
      className="hint"
      tabIndex={0}
      aria-label={text}
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
    >
      ?
      {open &&
        pos &&
        createPortal(
          <span className="hint-pop" role="tooltip" style={{ left: pos.left, top: pos.top, width: Math.min(WIDTH, window.innerWidth - 2 * MARGIN) }}>
            {text}
          </span>,
          document.body,
        )}
    </span>
  );
}
