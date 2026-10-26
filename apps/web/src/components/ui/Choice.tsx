import { useState, type ReactNode } from 'react';
import { Check, Copy } from 'lucide-react';

/**
 * Multi-select as toggle chips (regions, services, drain types). Same look as `Segmented`, but any number
 * can be pressed and the row wraps on narrow screens.
 */
export function ChoiceChips<T extends string>({ options, value, onChange, label }: {
  options: { id: T; label: ReactNode; hint?: string }[]; value: T[]; onChange: (v: T[]) => void; label: string;
}) {
  const toggle = (id: T) => onChange(value.includes(id) ? value.filter((x) => x !== id) : [...value, id]);
  return (
    <div className="segmented" role="group" aria-label={label} style={{ display: 'flex', flexWrap: 'wrap', width: 'fit-content', maxWidth: '100%' }}>
      {options.map((o) => {
        const on = value.includes(o.id);
        return (
          <button key={o.id} type="button" className="segmented__item" aria-pressed={on} title={o.hint} onClick={() => toggle(o.id)}
            style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)' }}>
            {on && <Check size={12} aria-hidden />}{o.label}
          </button>
        );
      })}
    </div>
  );
}

/** Code block with a copy button (snippets, URLs, policies the user pastes elsewhere). */
/** `wrap` breaks long single-line values (URLs, ARNs) instead of scrolling. */
export function CopyCode({ children, label = 'Copy', wrap }: { children: string; label?: string; wrap?: boolean }) {
  const [done, setDone] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(children).then(() => { setDone(true); setTimeout(() => setDone(false), 1500); }, () => undefined);
  };
  return (
    <div style={{ position: 'relative', minWidth: 0 }}>
      <pre className="code" style={{ paddingRight: 'calc(var(--control-h-sm) + var(--space-3))', ...(wrap ? { whiteSpace: 'pre-wrap', wordBreak: 'break-all' } : {}) }}>{children}</pre>
      <button type="button" className="btn btn--ghost btn--sm btn--icon" aria-label={done ? 'Copied' : label} title={done ? 'Copied' : label} onClick={copy}
        style={{ position: 'absolute', top: 'var(--space-1)', right: 'var(--space-1)' }}>
        {done ? <Check size={14} /> : <Copy size={14} />}
      </button>
    </div>
  );
}
