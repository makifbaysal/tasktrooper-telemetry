/** Replays tab of a client app (web / mobile): recorded sessions, newest first (ADR-021, product.md "Replays"). */
import { useState } from 'react';
import { useNavigate } from 'react-router';
import { PlayCircle, UserRound } from 'lucide-react';
import { Badge, Card, SearchInput, Segmented, StatusBadge, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { replayUrl, replaysFor, type ReplaySummary, type Service } from '@/data';
import { ago, clock, duration } from '@/lib/format';

export function ReplaysTab({ app }: { app: Service }) {
  const { range } = useApp();
  const nav = useNavigate();
  const [only, setOnly] = useState<'all' | 'errors'>('all');
  const [q, setQ] = useState('');
  const mobile = app.type === 'mobile';
  const rows = replaysFor(app.id, { range, q, hasErrors: only === 'errors' });
  return (
    <div className="stack" style={{ gap: 'var(--space-3)' }}>
      <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
        <Segmented label="Show" value={only} onChange={setOnly} options={[{ id: 'all', label: 'All recordings' }, { id: 'errors', label: 'Errors only' }]} />
        <div style={{ flex: '1 1 260px', maxWidth: 420 }}>
          <SearchInput placeholder={`Search user id or ${mobile ? 'screen' : 'page'}`} value={q} onChange={(e) => setQ(e.target.value)} aria-label="Search replays" />
        </div>
      </div>
      <Card flush subtitle={`Text and inputs are masked. The SDK records ${mobile ? 'a wireframe of the view hierarchy, never pixels' : 'the DOM with rrweb'}; by default 10% of sessions plus every session with an error.`}>
        <Table<ReplaySummary> rows={rows} rowKey={(r) => r.sessionId} defaultSort={{ key: 'start', dir: 'desc' }} onRowClick={(r) => nav(replayUrl(r.sessionId))}
          empty={<span className="muted">No recorded sessions{q || only === 'errors' ? ' match these filters' : ' in range'}.</span>}
          columns={[
            { key: 'start', header: 'Started', sortValue: (r) => r.startedAt, render: (r) => <span style={{ whiteSpace: 'nowrap' }}>{clock(r.startedAt)} <span className="muted">· {ago(r.startedAt)}</span></span> },
            { key: 'user', header: 'User', sortValue: (r) => r.user.id ?? r.user.anonymousId, render: (r) => (
              <span className="row" style={{ gap: 'var(--space-1)' }}><UserRound size={14} className="muted" aria-hidden />{r.user.id ?? <span className="muted">{r.user.anonymousId || 'anonymous'}</span>}</span>) },
            { key: 'device', header: mobile ? 'Device' : 'Device / browser', render: (r) => <span className="secondary">{[r.device, mobile ? r.os : r.browser].filter(Boolean).join(' · ') || '—'}{r.country ? <span className="muted"> · {r.country}</span> : null}</span> },
            { key: 'first', header: mobile ? 'First screen' : 'First page', sortValue: (r) => r.firstView, render: (r) => <span className="mono" style={{ fontSize: 'var(--text-xs)' }}>{r.firstView || '—'}</span> },
            { key: 'dur', header: 'Duration', numeric: true, sortValue: (r) => r.durationMs, render: (r) => duration(r.durationMs / 1000) },
            { key: 'views', header: mobile ? 'Screens' : 'Views', numeric: true, sortValue: (r) => r.views, render: (r) => r.views },
            { key: 'err', header: 'Errors', numeric: true, sortValue: (r) => r.errors, render: (r) => r.errors ? <StatusBadge status="warning" label={String(r.errors)} /> : <span className="muted">0</span> },
            { key: 'rage', header: mobile ? 'Rage taps' : 'Rage clicks', numeric: true, sortValue: (r) => r.rageClicks, render: (r) => r.rageClicks ? <Badge tone="warning">{r.rageClicks}</Badge> : <span className="muted">0</span> },
            { key: 'go', header: '', render: () => <PlayCircle size={16} className="muted" aria-hidden /> },
          ]} />
      </Card>
    </div>
  );
}
