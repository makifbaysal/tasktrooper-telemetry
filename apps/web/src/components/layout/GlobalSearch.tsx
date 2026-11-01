import { useEffect, useId, useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router';
import { Search, type LucideIcon } from 'lucide-react';
import { services, useData } from '@/data';
import { SERVICE_CATEGORIES } from '@/screens/shared';

export interface SearchPage { to: string; label: string; icon: LucideIcon }
interface Hit { key: string; to: string; label: string; hint: string; icon?: LucideIcon }

const MAX = 8;

/**
 * Topbar quick jump: services (by name) and pages. `/` or ⌘K / Ctrl+K focuses it; arrows move, Enter opens,
 * Escape clears. Combobox + listbox semantics so screen readers announce the active option.
 */
export function GlobalSearch({ pages }: { pages: SearchPage[] }) {
  useData();
  const nav = useNavigate();
  const listId = useId();
  const input = useRef<HTMLInputElement>(null);
  const [q, setQ] = useState('');
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing = e.target instanceof HTMLElement && e.target.closest('input, textarea, select, [contenteditable="true"]');
      if ((e.key === 'k' && (e.metaKey || e.ctrlKey)) || (e.key === '/' && !typing)) {
        e.preventDefault();
        input.current?.focus();
        input.current?.select();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const hits = useMemo<Hit[]>(() => {
    const term = q.trim().toLowerCase();
    if (!term) return [];
    const typeLabel = (t: string) => SERVICE_CATEGORIES.find((c) => c.types.includes(t as never))?.label ?? t;
    const svc = services
      .filter((s) => s.name.toLowerCase().includes(term))
      .sort((a, b) => Number(!a.name.toLowerCase().startsWith(term)) - Number(!b.name.toLowerCase().startsWith(term)) || a.name.localeCompare(b.name))
      .map((s) => ({ key: `s:${s.id}`, to: `/services/${s.id}`, label: s.name, hint: typeLabel(s.type) }));
    const pg = pages
      .filter((p) => p.label.toLowerCase().includes(term))
      .map((p) => ({ key: `p:${p.to}`, to: p.to, label: p.label, hint: 'Page', icon: p.icon }));
    return [...svc, ...pg].slice(0, MAX);
  }, [q, pages]);

  const go = (h: Hit) => {
    nav(h.to);
    setQ('');
    setOpen(false);
    input.current?.blur();
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); setOpen(true); setActive((i) => Math.min(i + 1, hits.length - 1)); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); setActive((i) => Math.max(i - 1, 0)); }
    else if (e.key === 'Enter' && hits[active]) { e.preventDefault(); go(hits[active]); }
    else if (e.key === 'Escape') { setQ(''); setOpen(false); input.current?.blur(); }
  };

  const show = open && q.trim() !== '';

  return (
    <div className="global-search">
      <div className="input-group">
        <Search size={14} aria-hidden />
        <input ref={input} className="input" type="search" placeholder="Jump to a service or page" aria-label="Jump to a service or page"
          role="combobox" aria-expanded={show} aria-controls={listId} aria-autocomplete="list"
          aria-activedescendant={show && hits[active] ? `${listId}-${active}` : undefined}
          value={q} onChange={(e) => { setQ(e.target.value); setActive(0); setOpen(true); }}
          onFocus={() => setOpen(true)} onBlur={() => setOpen(false)} onKeyDown={onKeyDown} />
        <kbd className="global-search__kbd" aria-hidden>⌘K</kbd>
      </div>
      {show && (
        <ul className="global-search__list" id={listId} role="listbox" aria-label="Results">
          {hits.length === 0 && <li className="global-search__empty">No service or page matches “{q.trim()}”.</li>}
          {hits.map((h, i) => (
            <li key={h.key} id={`${listId}-${i}`} role="option" aria-selected={i === active} className="global-search__item"
              onMouseDown={(e) => { e.preventDefault(); go(h); }} onMouseEnter={() => setActive(i)}>
              {h.icon && <h.icon size={14} aria-hidden />}
              <span className="truncate">{h.label}</span>
              <span className="global-search__hint">{h.hint}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
