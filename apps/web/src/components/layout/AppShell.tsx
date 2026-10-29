import { useLayoutEffect, useState } from 'react';
import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router';
import {
  AlertOctagon, BookOpen, BotMessageSquare, Boxes, Server, Building2, ChevronsUpDown, Code2, Database, Gauge, KeyRound, LayoutDashboard, LogOut, Moon, Waypoints,
  Layers, PanelLeftClose, PanelLeftOpen, Plug, Settings, Sparkles, SquareFunction, Sun, SunMoon, Target, UserRound, Users, type LucideIcon,
} from 'lucide-react';
import { Avatar, Badge, Menu, MenuDivider, MenuItem, MenuLabel, Segmented, Select } from '@/components/ui';
import { logout, switchProject, useAuth } from '@/data/auth';
import { PROVIDERS, clusterHealth, clusters, databases, hosts, incidents, integrations, k8sClusters, kpis, services, useData, worst, type Health, type TimeRange } from '@/data';
import { useAcks } from '@/screens/Alerts';
import { SERVICE_CATEGORIES } from '@/screens/shared';
import { AiQuotaBanner } from '@/screens/Usage';
import { useApp } from './context';
import { LiquidCursor } from './LiquidCursor';

/** `badge` is evaluated on every render so counts follow data changes (api hydrate, mutations). */
interface NavItem { to: string; label: string; icon: LucideIcon; end?: boolean; badge?: () => { count: number; tone?: Health }; liveCount?: 'firing' }
const NAV: { section: string | null; items: NavItem[] }[] = [
  { section: null, items: [
    { to: '/', label: 'Overview', icon: LayoutDashboard, end: true },
    { to: '/incidents', label: 'Incidents', icon: AlertOctagon, liveCount: 'firing' },
  ] },
  { section: 'Infrastructure', items: [
    { to: '/infrastructure/databases', label: 'Databases', icon: Database,
      badge: () => ({ count: databases.filter((d) => d.status !== 'ok').length, tone: worst(databases.map((d) => d.status)) }) },
    { to: '/infrastructure/queues', label: 'Message queues', icon: Waypoints,
      badge: () => ({ count: clusters.filter((c) => clusterHealth(c) !== 'ok').length, tone: worst(clusters.map(clusterHealth)) }) },
    { to: '/infrastructure/hosts', label: 'Hosts', icon: Server,
      badge: () => ({ count: hosts.filter((h) => h.status === 'critical' || h.status === 'warning').length, tone: hosts.some((h) => h.status === 'critical') ? 'critical' : 'warning' }) },
    { to: '/infrastructure/kubernetes', label: 'Kubernetes', icon: Boxes,
      badge: () => ({ count: k8sClusters.filter((c) => c.status === 'critical' || c.status === 'warning').length, tone: k8sClusters.some((c) => c.status === 'critical') ? 'critical' : 'warning' }) },
    { to: '/infrastructure/serverless', label: 'Serverless', icon: SquareFunction },
  ] },
  { section: 'Objectives', items: [
    { to: '/kpis', label: 'KPIs', icon: Target, badge: () => ({ count: kpis.filter((k) => k.status !== 'ok').length }) },
  ] },
  // customer LLM calls (ADR-023) — not "AI usage" (our own quota, user menu)
  { section: 'AI', items: [
    { to: '/ai', label: 'AI monitoring', icon: BotMessageSquare },
  ] },
  { section: 'Tools', items: [
    { to: '/load-tests', label: 'Load tests', icon: Gauge },
    { to: '/code', label: 'Code insights', icon: Code2 },
  ] },
];

const RANGES: { id: TimeRange; label: string }[] = [
  { id: '15m', label: '15m' }, { id: '30m', label: '30m' }, { id: '1h', label: '1h' }, { id: '6h', label: '6h' }, { id: '24h', label: '24h' }, { id: '7d', label: '7d' },
];

export function AppShell() {
  useCardReveal();
  const { range, setRange, env, setEnv, theme, setTheme } = useApp();
  const [sidebarOpen, setSidebarOpen] = useState(() => {
    const saved = window.localStorage.getItem('tasktrooper.sidebarOpen');
    return saved === null ? !window.matchMedia('(max-width: 900px)').matches : saved !== 'false';
  });
  const toggleSidebar = () => setSidebarOpen((open) => {
    window.localStorage.setItem('tasktrooper.sidebarOpen', String(!open));
    return !open;
  });
  const nextTheme = theme === 'system' ? 'light' : theme === 'light' ? 'dark' : 'system';
  const ThemeIcon = theme === 'system' ? SunMoon : theme === 'light' ? Sun : Moon;

  return (
    <div className={`shell${sidebarOpen ? '' : ' shell--sidebar-collapsed'}`}>
      <LiquidCursor />
      <aside className="sidebar" id="app-sidebar" aria-label="Main navigation"
        onClick={(event) => {
          if (event.target instanceof Element && event.target.closest('a') && window.matchMedia('(max-width: 900px)').matches) {
            setSidebarOpen(false);
            window.localStorage.setItem('tasktrooper.sidebarOpen', 'false');
          }
        }}>
        <button className="btn btn--ghost sidebar__close" type="button" onClick={toggleSidebar} aria-label="Close navigation">
          <PanelLeftClose size={18} aria-hidden /> Close navigation
        </button>
        <ProjectSwitcher />
        {NAV.slice(0, 1).map((g, gi) => <NavGroup key={gi} g={g} />)}
        <ServicesNav />
        {NAV.slice(1).map((g, gi) => <NavGroup key={gi} g={g} />)}
        <div className="sidebar__footer">
          <div className="sidebar__section">Settings</div>
          <div className="sidebar__grid">
            <NavLink to="/settings" end className="sidebar__link sidebar__tile" title="Settings & SDK" aria-label="Settings & SDK"><Settings size={20} aria-hidden /><span className="sidebar__label">Settings &amp; SDK</span></NavLink>
            <NavLink to="/settings/integrations" className="sidebar__link sidebar__tile" title="Integrations" aria-label="Integrations"><Plug size={20} aria-hidden /><span className="sidebar__label">Integrations</span><IntegrationErrors /></NavLink>
            <NavLink to="/design-system" className="sidebar__link sidebar__tile" title="Design system" aria-label="Design system"><BookOpen size={20} aria-hidden /><span className="sidebar__label">Design system</span></NavLink>
          </div>
        </div>
      </aside>

      <div className="main">
        <header className="topbar">
          <button className="btn btn--ghost btn--icon sidebar-toggle" type="button" title={sidebarOpen ? 'Collapse sidebar' : 'Expand sidebar'}
            aria-label={sidebarOpen ? 'Collapse sidebar' : 'Expand sidebar'} aria-expanded={sidebarOpen} aria-controls="app-sidebar" onClick={toggleSidebar}>
            {sidebarOpen ? <PanelLeftClose size={18} aria-hidden /> : <PanelLeftOpen size={18} aria-hidden />}
          </button>
          <span className="spacer" />
          <Select aria-label="Environment" value={env} onChange={(e) => setEnv(e.target.value)}
            options={[{ value: 'production', label: 'production' }, { value: 'staging', label: 'staging' }]} />
          <Segmented label="Time range" options={RANGES} value={range} onChange={setRange} />
          <button className="btn btn--ghost btn--icon" title={`Theme: ${theme}`} aria-label={`Theme: ${theme}. Switch to ${nextTheme}`} onClick={() => setTheme(nextTheme)}>
            <ThemeIcon size={16} />
          </button>
          <UserMenu />
        </header>
        <main className="content">
          <AiQuotaBanner />
          <Outlet />
        </main>
      </div>
    </div>
  );
}

function useCardReveal() {
  const location = useLocation();

  useLayoutEffect(() => {
    const content = document.querySelector<HTMLElement>('.content');
    if (!content || window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    const seen = new WeakSet<Element>();
    let revealed = 0;
    let seenCount = 0;
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        const card = entry.target as HTMLElement;
        if (!entry.isIntersecting) {
          delete card.dataset.inView;
          const bounds = card.getBoundingClientRect();
          card.dataset.revealFrom = bounds.bottom <= 0 ? 'above' : 'below';
          card.dataset.reveal = 'pending';
          continue;
        }
        card.dataset.inView = 'true';
        if (card.dataset.reveal === 'pending') {
          card.style.setProperty('--reveal-delay', `${Math.min(revealed % 3, 2) * 75}ms`);
          revealed += 1;
          card.dataset.reveal = 'visible';
        }
      }
    }, { threshold: 0 });

    let frame = 0;
    const collect = () => {
      frame = 0;
      for (const card of content.querySelectorAll<HTMLElement>('.card')) {
        if (seen.has(card)) continue;
        seen.add(card);
        const bounds = card.getBoundingClientRect();
        card.style.setProperty('--ambient-offset', `${-(seenCount++ % 5) * 1.1}s`);
        card.dataset.revealFrom = 'below';
        card.dataset.reveal = bounds.top < window.innerHeight * 0.85 || bounds.bottom < 0 ? 'settled' : 'pending';
        observer.observe(card);
      }
    };
    const changes = new MutationObserver(() => {
      if (!frame) frame = requestAnimationFrame(collect);
    });
    collect();
    changes.observe(content, { childList: true, subtree: true });
    return () => {
      changes.disconnect();
      observer.disconnect();
      if (frame) cancelAnimationFrame(frame);
    };
  }, [location.pathname]);
}

function NavGroup({ g }: { g: (typeof NAV)[number] }) {
  useData();
  return (
    <nav aria-label={g.section ?? 'Main'}>
      <div className="sidebar__section">{g.section ?? 'Workspace'}</div>
      <div className="sidebar__grid">
        {g.items.map((it) => (
          <NavLink key={it.to} to={it.to} end={it.end ?? false} className="sidebar__link sidebar__tile" title={it.label} aria-label={it.label}>
            <it.icon size={20} aria-hidden />
            <span className="sidebar__label">{it.label}</span>
            {it.liveCount ? <FiringCount /> : <NavCount badge={it.badge} />}
          </NavLink>
        ))}
      </div>
    </nav>
  );
}

function NavCount({ badge }: { badge?: NavItem['badge'] }) {
  const b = badge?.();
  return b?.count ? <span className="sidebar__count"><Badge tone={b.tone === 'warning' ? 'warning' : 'critical'}>{b.count}</Badge></span> : null;
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
      <div className="sidebar__grid">
        <Link to="/services" className="sidebar__link sidebar__tile" title="All services" aria-label="All services" aria-current={onList && !type ? 'page' : undefined}>
          <Layers size={20} aria-hidden />
          <span className="sidebar__label">All services</span>
          <span className="sidebar__count muted num">{services.length}</span>
        </Link>
        {items.map((c) => (
          <Link key={c.id} to={`/services?type=${c.id}`} className="sidebar__link sidebar__tile" aria-label={c.label} aria-current={c.active ? 'page' : undefined}
            title={c.bad.length ? `${c.bad.length} degraded` : undefined}>
            <c.Icon size={20} aria-hidden />
            <span className="sidebar__label">{c.label}</span>
            <span className="sidebar__count">
              {c.bad.length > 0
                ? <Badge tone={c.critical ? 'critical' : 'warning'}>{c.bad.length}</Badge>
                : <span className="muted num">{c.rows.length}</span>}
            </span>
          </Link>
        ))}
      </div>
    </nav>
  );
}

/** Integrations whose sync is failing. */
function IntegrationErrors() {
  useData();
  const n = integrations.filter((i) => i.status === 'error' && PROVIDERS.includes(i.provider)).length;
  return n ? <span className="sidebar__count"><Badge tone="critical">{n}</Badge></span> : null;
}

/** Org + project switcher (ADR-016). Switching re-scopes every request (X-Trooper-Project), drops the cache and rehydrates. */
function ProjectSwitcher() {
  const { me, org, project, mode } = useAuth();
  const [busy, setBusy] = useState(false);
  const pick = (id: string) => { setBusy(true); void switchProject(id).finally(() => setBusy(false)); };
  const label = (
    <>
      <span className="dot" style={{ background: 'var(--series-3)' }} />
      <span style={{ flex: 1, minWidth: 0 }}>
        <div className="truncate" style={{ fontWeight: 'var(--weight-medium)' }}>{org?.name ?? (mode === 'key' ? 'API key' : 'Project')}</div>
        <div className="muted truncate" style={{ fontSize: 'var(--text-xs)' }}>{busy ? 'Switching…' : project?.name ?? (mode === 'key' ? 'project of the key' : '—')}</div>
      </span>
    </>
  );
  // api-key mode: the key selects the project, nothing to switch
  if (!me || me.orgs.flatMap((o) => o.projects).length < 2) return <div className="sidebar__project" style={{ cursor: 'default' }}>{label}</div>;
  return (
    <Menu label="Switch project" block trigger={({ open, toggle }) => (
      <button className="sidebar__project" aria-haspopup="menu" aria-expanded={open} onClick={toggle}>
        {label}
        <ChevronsUpDown size={14} className="muted" />
      </button>
    )}>
      {(close) => me.orgs.map((o, i) => (
        <div key={o.id}>
          {i > 0 && <MenuDivider />}
          <MenuLabel>{o.name} · {o.role}</MenuLabel>
          {o.projects.map((p) => (
            <MenuItem key={p.id} icon={<Building2 size={14} />} selected={p.id === project?.id} onSelect={() => { close(); pick(p.id); }}>{p.name}</MenuItem>
          ))}
        </div>
      ))}
    </Menu>
  );
}

/** Avatar → account, role, settings shortcuts, sign out. */
function UserMenu() {
  const { me, org, role, mode } = useAuth();
  const nav = useNavigate();
  const name = me ? me.user.name ?? me.user.email : 'API key';
  return (
    <Menu label="Account" align="end" trigger={({ open, toggle }) => (
      <button className="btn btn--ghost user-trigger" aria-haspopup="menu" aria-expanded={open} aria-label={`Account: ${name}`} onClick={toggle}>
        {me ? <Avatar name={name} src={me.user.avatarUrl} /> : <span className="avatar avatar--md"><KeyRound size={14} /></span>}
      </button>
    )}>
      {(close) => (
        <>
          <div className="row" style={{ padding: 'var(--space-2)', gap: 'var(--space-3)' }}>
            {me ? <Avatar name={name} src={me.user.avatarUrl} size="lg" /> : <span className="avatar avatar--lg"><KeyRound size={16} /></span>}
            <div style={{ minWidth: 0 }}>
              <div className="truncate" style={{ fontWeight: 'var(--weight-medium)' }}>{name}</div>
              <div className="muted truncate" style={{ fontSize: 'var(--text-sm)' }}>{me ? me.user.email : mode === 'key' ? 'Signed in with an api key' : 'Mock data'}</div>
              {role && <div style={{ marginTop: 'var(--space-1)' }}><Badge tone={role === 'viewer' ? 'neutral' : 'accent'}>{org?.name} · {role}</Badge></div>}
            </div>
          </div>
          <MenuDivider />
          <MenuItem icon={<UserRound size={14} />} onSelect={() => { close(); nav('/settings'); }}>Settings &amp; SDK</MenuItem>
          <MenuItem icon={<Users size={14} />} onSelect={() => { close(); nav('/settings?tab=members'); }}>Members</MenuItem>
          <MenuItem icon={<KeyRound size={14} />} onSelect={() => { close(); nav('/settings?tab=keys'); }}>API keys</MenuItem>
          <MenuItem icon={<Sparkles size={14} />} onSelect={() => { close(); nav('/settings/usage'); }}>AI usage</MenuItem>
          {mode === 'session' && <>
            <MenuDivider />
            <MenuItem icon={<LogOut size={14} />} onSelect={() => { close(); void logout(); }}>Sign out</MenuItem>
          </>}
        </>
      )}
    </Menu>
  );
}
