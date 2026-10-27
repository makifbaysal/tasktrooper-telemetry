import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Check } from 'lucide-react';

/**
 * Popover menu anchored to a trigger (user menu, org/project switcher). Esc, outside click or picking an item closes it.
 * `trigger` receives the open state so it can render its own button; items are `MenuItem` / `MenuLabel` / `MenuDivider`.
 */
export function Menu({ trigger, children, align = 'start', placement = 'below', label, block }: {
  trigger: (p: { open: boolean; toggle: () => void }) => ReactNode;
  children: ReactNode | ((close: () => void) => ReactNode);
  align?: 'start' | 'end';
  /** `above` for triggers at the bottom of the screen */
  placement?: 'below' | 'above';
  label: string;
  /** full-width anchor (sidebar project switcher) */
  block?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => { if (!root.current?.contains(e.target as Node)) setOpen(false); };
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey); };
  }, [open]);
  const close = () => setOpen(false);
  return (
    <div className={block ? 'menu-anchor menu-anchor--block' : 'menu-anchor'} ref={root}>
      {trigger({ open, toggle: () => setOpen((o) => !o) })}
      {open && (
        <div className={`menu menu--${align} menu--${placement}`} role="menu" aria-label={label}>
          {typeof children === 'function' ? children(close) : children}
        </div>
      )}
    </div>
  );
}

export function MenuItem({ icon, children, hint, selected, disabled, danger, onSelect }: {
  icon?: ReactNode; children: ReactNode; hint?: ReactNode; selected?: boolean; disabled?: boolean; danger?: boolean; onSelect: () => void;
}) {
  return (
    <button type="button" role="menuitem" className={['menu__item', danger && 'menu__item--danger'].filter(Boolean).join(' ')}
      aria-checked={selected} disabled={disabled} onClick={onSelect}>
      <span className="menu__icon">{selected ? <Check size={14} aria-hidden /> : icon}</span>
      <span className="menu__text">{children}{hint && <span className="menu__hint">{hint}</span>}</span>
    </button>
  );
}

export function MenuLabel({ children }: { children: ReactNode }) {
  return <div className="menu__label">{children}</div>;
}

export function MenuDivider() {
  return <div className="menu__divider" role="separator" />;
}

/** User initials on a neutral disc (or the identity provider's picture). */
export function Avatar({ name, src, size = 'md' }: { name: string; src?: string; size?: 'sm' | 'md' | 'lg' }) {
  const initials = name.split(/[\s@._-]+/).filter(Boolean).slice(0, 2).map((p) => p[0]!.toUpperCase()).join('') || '?';
  return (
    <span className={`avatar avatar--${size}`} aria-hidden>
      {src ? <img src={src} alt="" referrerPolicy="no-referrer" /> : initials}
    </span>
  );
}
