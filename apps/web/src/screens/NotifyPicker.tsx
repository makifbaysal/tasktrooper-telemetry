import { useState } from 'react';
import { Hash, Lock, Plus, X } from 'lucide-react';
import { useData } from '@/data';
import { connectSlack, parseTarget, slackChannels, slackTarget, slackWorkspace, type SlackChannel } from '@/data/slack';

/**
 * Notification targets of a monitor (ADR-017): Slack channels picked from the connected workspace
 * (`slack:<id>#<name>`, shown as `#name` chips, private ones with a lock) plus free-text targets
 * (pagerduty:…, email:…, webhook URLs). Enter adds what is typed; ⌫ on an empty input removes the last chip.
 */
export function NotifyPicker({ value, onChange, disabled }: { value: string[]; onChange: (v: string[]) => void; disabled?: boolean }) {
  useData();
  const [q, setQ] = useState('');
  const [focus, setFocus] = useState(false);
  const ws = slackWorkspace();
  const channels = ws ? slackChannels(q) : [];
  const privateIds = new Set((ws ? slackChannels('') : []).filter((c) => c.isPrivate).map((c) => c.id));
  const matches = channels.filter((c) => !value.includes(slackTarget(c))).slice(0, 8);

  const add = (t: string) => { const v = t.trim(); if (v && !value.includes(v)) onChange([...value, v]); setQ(''); };
  const pick = (c: SlackChannel) => add(slackTarget(c));
  const remove = (t: string) => onChange(value.filter((x) => x !== t));

  return (
    <div className="stack" style={{ gap: 'var(--space-2)' }}>
      <div className="notify">
        {value.map((t) => {
          const p = parseTarget(t);
          return (
            <span key={t} className="notify__chip" title={t}>
              {p.kind === 'slack' ? (privateIds.has(p.id) ? <Lock size={12} aria-label="Private channel" /> : <Hash size={12} aria-hidden />) : null}
              <span className={p.kind === 'slack' ? undefined : 'mono'}>{p.kind === 'slack' ? p.name : t}</span>
              {!disabled && <button type="button" aria-label={`Remove ${t}`} onClick={() => remove(t)}><X size={12} /></button>}
            </span>
          );
        })}
        {!disabled && (
          <input className="notify__input" value={q} placeholder={value.length ? 'Add…' : ws ? 'Search Slack channels or type pagerduty:, email:, URL' : 'pagerduty:service, email:a@b.com, https://…'}
            onChange={(e) => setQ(e.target.value)} onFocus={() => setFocus(true)} onBlur={() => setTimeout(() => setFocus(false), 150)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') { e.preventDefault(); if (matches.length && q.trim()) pick(matches[0]); else add(q); }
              if (e.key === 'Backspace' && !q && value.length) remove(value[value.length - 1]);
            }} />
        )}
        {focus && !disabled && ws && (matches.length > 0 || q) && (
          <div className="menu menu--start menu--below" role="listbox" aria-label="Slack channels" style={{ width: '100%', maxWidth: 'none' }}>
            {matches.map((c) => (
              <button key={c.id} type="button" role="option" aria-selected={false} className="menu__item" onMouseDown={(e) => e.preventDefault()} onClick={() => pick(c)}>
                <span className="menu__icon">{c.isPrivate ? <Lock size={14} aria-label="Private" /> : <Hash size={14} aria-hidden />}</span>
                <span className="menu__text">{c.name}{(!c.isMember || c.isPrivate) && <span className="menu__hint">{c.isPrivate ? 'private' : ''}{c.isPrivate && !c.isMember ? ' · ' : ''}{!c.isMember ? 'bot not in channel — posts via chat:write.public' : ''}</span>}</span>
              </button>
            ))}
            {q.trim() && (
              <button type="button" className="menu__item" onMouseDown={(e) => e.preventDefault()} onClick={() => add(q)}>
                <span className="menu__icon"><Plus size={14} /></span>
                <span className="menu__text"><span>Add “<span className="mono">{q.trim()}</span>” as a target</span></span>
              </button>
            )}
          </div>
        )}
      </div>
      {!ws ? (
        <span className="field__hint">Slack isn’t connected — <a href="#" onClick={(e) => { e.preventDefault(); connectSlack(); }}>Connect Slack</a> to pick channels from a list.</span>
      ) : ws ? <span className="field__hint">Slack workspace {ws.teamName}</span> : null}
    </div>
  );
}
