import { Link, NavLink, Outlet, useLocation } from 'react-router';
import {
  AlertOctagon, BookOpen, ChevronsUpDown, Code2, Database, Gauge, LayoutDashboard, Moon, Waypoints,
  Layers, Plug, Search, Settings, Sun, SunMoon, Target, Zap, type LucideIcon,
} from 'lucide-react';
import { Badge, Segmented, Select } from '@/components/ui';
import { clusterHealth, clusters, databases, incidents, integrations, kpis, services, useData, worst, type Health, type TimeRange } from '@/data';
import { useAcks } from '@/screens/Alerts';
import { SERVICE_CATEGORIES } from '@/screens/shared';
import { useApp } from './context';
import { Logo } from './Logo';

interface NavItem { to: string; label: string; icon: LucideIcon; end?: boolean; count?: number; countTone?: Health; liveCount?: 'firing' }
const NAV: { section: string | null; items: NavItem[] }[] = [
  { section: null, items: [
    { to: '/', label: 'Overview', icon: LayoutDashboard, end: true },
    { to: '/incidents', label: 'Incidents', icon: AlertOctagon, liveCount: 'firing' },
  ] },
  { section: 'Infrastructure', items: [
    { to: '/infrastructure/databases', label: 'Databases', icon: Database,
      count: databases.filter((d) => d.status !== 'ok').length, countTone: worst(databases.map((d) => d.status)) },
    { to: '/infrastructure/queues', label: 'Message queues', icon: Waypoints,
      count: clusters.filter((c) => clusterHealth(c) !== 'ok').length, countTone: worst(clusters.map(clusterHealth)) },
  ] },
  { section: 'Objectives', items: [
    { to: '/kpis', label: 'KPIs', icon: Target, count: kpis.filter((k) => k.status !== 'ok').length },
  ] },
  { section: 'Tools', items: [
    { to: '/load-tests', label: 'Load tests', icon: Gauge },
    { to: '/code', label: 'Code insights', icon: Code2 },
  ] },
];

const RANGES: { id: TimeRange; label: string }[] = [
  { id: '1h', label: '1h' }, { id: '6h', label: '6h' }, { id: '24h', label: '24h' }, { id: '7d', label: '7d' },
];

export function AppShell() {
  const { range, setRange, env, setEnv, theme, setTheme } = useApp();
  const nextTheme = theme === 'system' ? 'light' : theme === 'light' ? 'dark' : 'system';
  const ThemeIcon = theme === 'system' ? SunMoon : theme === 'light' ? Sun : Moon;

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="sidebar__brand">
          <span className="sidebar__logo"><Logo /></span>
          <span className="sidebar__name"><strong>TaskTrooper</strong><span>Telemetry</span></span>
        </div>
        <button className="sidebar__project">
          <span className="dot" style={{ background: 'var(--series-3)' }} />
          <span style={{ flex: 1 }}>
            <div style={{ fontWeight: 500 }}>Shopco</div>
            <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>shop-platform</div>
          </span>
          <ChevronsUpDown size={14} className="muted" />
        </button>
        {NAV.slice(0, 1).map((g, gi) => <NavGroup key={gi} g={g} />)}
        <ServicesNav />
        {NAV.slice(1).map((g, gi) => <NavGroup key={gi} g={g} />)}
        <div className="sidebar__footer">
          <NavLink to="/settings" end className="sidebar__link"><Settings size={16} aria-hidden />Settings &amp; SDK</NavLink>
          <NavLink to="/settings/integrations" className="sidebar__link"><Plug size={16} aria-hidden />Integrations<IntegrationErrors /></NavLink>
          <NavLink to="/design-system" className="sidebar__link"><BookOpen size={16} aria-hidden />Design system</NavLink>
        </div>
      </aside>

      <div className="main">
        <header className="topbar">
          <div className="input-group" style={{ width: 280, maxWidth: '40vw' }}>
            <Search size={14} aria-hidden />
            <input className="input" placeholder="Search services, endpoints, pages…" style={{ width: '100%' }} />
          </div>
          <span className="spacer" />
          <Select aria-label="Environment" value={env} onChange={(e) => setEnv(e.target.value)}
            options={[{ value: 'production', label: 'production' }, { value: 'staging', label: 'staging' }]} />
          <Segmented label="Time range" options={RANGES} value={range} onChange={setRange} />
          <button className="btn btn--ghost btn--icon" title={`Theme: ${theme}`} aria-label={`Theme: ${theme}. Switch to ${nextTheme}`} onClick={() => setTheme(nextTheme)}>
            <ThemeIcon size={16} />
          </button>
          <button className="btn btn--ghost btn--icon" aria-label="What's new"><Zap size={16} /></button>
        </header>
        <main className="content">
          <Outlet />
        </main>
      </div>
    </div>
  );
}

function NavGroup({ g }: { g: (typeof NAV)[number] }) {
  return (
    <nav aria-label={g.section ?? 'Main'}>
      {g.section && <div className="sidebar__section">{g.section}</div>}
      {g.items.map((it) => (
        <NavLink key={it.to} to={it.to} end={it.end ?? false} className="sidebar__link">
          <it.icon size={16} aria-hidden />
          {it.label}
          {it.liveCount ? <FiringCount /> : it.count ? <span className="sidebar__count"><Badge tone={it.countTone === 'warning' ? 'warning' : 'critical'}>{it.count}</Badge></span> : null}
        </NavLink>
      ))}
    </nav>
  );
}

/** Firing incidents; drops as soon as one is acknowledged anywhere in the app. */
function FiringCount() {
  const { stateOf } = useAcks();
  const n = incidents.filter((i) => stateOf(i) === 'firing').length;
  return n ? <span className="sidebar__count"><Badge tone="critical">{n}</Badge></span> : null;
}

/** One entry per service category; a service or endpoint page highlights its own category. */
function ServicesNav() {
  const { pathname, search } = useLocation();
  const type = new URLSearchParams(search).get('type');
  const detailId = pathname.match(/^\/services\/([^/]+)/)?.[1];
  const detailType = services.find((s) => s.id === detailId)?.type;
  const onList = pathname === '/services';

  const items = SERVICE_CATEGORIES.map((c) => {
    const rows = services.filter((s) => c.types.includes(s.type));
    const bad = rows.filter((s) => s.status !== 'ok');
    const active = (onList && type === c.id) || (!!detailType && c.types.includes(detailType));
    return { ...c, rows, bad, critical: bad.some((s) => s.status === 'critical'), active };
  }).filter((c) => c.rows.length > 0);

  return (
    <nav aria-label="Services">
      <div className="sidebar__section">Services</div>
      <Link to="/services" className="sidebar__link" aria-current={onList && !type ? 'page' : undefined}>
        <Layers size={16} aria-hidden />All services
        <span className="sidebar__count muted num">{services.length}</span>
      </Link>
      {items.map((c) => (
        <Link key={c.id} to={`/services?type=${c.id}`} className="sidebar__link" aria-current={c.active ? 'page' : undefined}
          title={c.bad.length ? `${c.bad.length} degraded` : undefined}>
          <c.Icon size={16} aria-hidden />
          {c.label}
          <span className="sidebar__count">
            {c.bad.length > 0
              ? <Badge tone={c.critical ? 'critical' : 'warning'}>{c.bad.length}</Badge>
              : <span className="muted num">{c.rows.length}</span>}
          </span>
        </Link>
      ))}
    </nav>
  );
}

/** Integrations whose sync is failing. */
function IntegrationErrors() {
  useData();
  const n = integrations.filter((i) => i.status === 'error').length;
  return n ? <span className="sidebar__count"><Badge tone="critical">{n}</Badge></span> : null;
}
