import { useEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { X } from 'lucide-react';

interface Props {
  open: boolean;
  onClose: () => void;
  title: ReactNode;
  subtitle?: ReactNode;
  /** header actions, e.g. "Open full page" */
  actions?: ReactNode;
  width?: number;
  children: ReactNode;
}

/**
 * Right-side detail panel. The standard way to show "details on click" without leaving the page.
 * Esc / backdrop click closes. Content scrolls; header stays.
 */
export function Drawer({ open, onClose, title, subtitle, actions, width = 640, children }: Props) {
  const panel = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose();
    document.addEventListener('keydown', onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    panel.current?.focus();
    return () => {
      document.removeEventListener('keydown', onKey);
      document.body.style.overflow = prev;
    };
  }, [open, onClose]);

  if (!open) return null;
  return createPortal(
    <div className="drawer-root">
      <div className="drawer-backdrop" onClick={onClose} />
      <div ref={panel} className="drawer" role="dialog" aria-modal="true" tabIndex={-1} style={{ width: `min(${width}px, 100vw)` }}>
        <header className="drawer__head">
          <div className="drawer__titles">
            <h2 className="drawer__title">{title}</h2>
            {subtitle && <div className="drawer__subtitle">{subtitle}</div>}
          </div>
          <div className="drawer__actions">
            {actions}
            <button className="btn btn--ghost btn--icon" aria-label="Close" onClick={onClose}><X size={16} /></button>
          </div>
        </header>
        <div className="drawer__body">{children}</div>
      </div>
    </div>,
    document.body,
  );
}

/** `const d = useDrawer<Row>(); d.open(row); <Drawer open={!!d.item} onClose={d.close}>…` */
export function useDrawer<T>() {
  const [item, setItem] = useState<T | null>(null);
  return { item, open: setItem, close: () => setItem(null) };
}

/** Titled block inside a Drawer (or anywhere): small heading + content. */
export function DrawerSection({ title, aside, children }: { title: ReactNode; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="drawer__section">
      <div className="drawer__section-head"><h3>{title}</h3>{aside && <span className="muted">{aside}</span>}</div>
      {children}
    </section>
  );
}

/** Label / value grid for facts. */
export function KeyValues({ items }: { items: [ReactNode, ReactNode][] }) {
  return (
    <dl className="kv">
      {items.map(([k, v], i) => (
        <div key={i} className="kv__row"><dt>{k}</dt><dd>{v}</dd></div>
      ))}
    </dl>
  );
}
