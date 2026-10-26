import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { ChevronRight } from 'lucide-react';

export function PageHeader({ title, subtitle, crumbs, actions }: {
  title: ReactNode; subtitle?: ReactNode; crumbs?: { label: string; to?: string }[]; actions?: ReactNode;
}) {
  return (
    <div className="page-header">
      <div className="page-header__text">
        {crumbs && (
          <nav className="page-header__crumbs" aria-label="Breadcrumb">
            {crumbs.map((c, i) => (
              <span key={i} className="row" style={{ gap: 4 }}>
                {c.to ? <Link to={c.to}>{c.label}</Link> : c.label}
                {i < crumbs.length - 1 && <ChevronRight size={12} aria-hidden />}
              </span>
            ))}
          </nav>
        )}
        <h1>{title}</h1>
        {subtitle && <p className="page-header__subtitle">{subtitle}</p>}
      </div>
      {actions && <div className="page-header__actions">{actions}</div>}
    </div>
  );
}

export function EmptyState({ icon, title, children, action }: { icon: ReactNode; title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      <div className="empty__icon">{icon}</div>
      <h3 style={{ color: 'var(--text-primary)' }}>{title}</h3>
      {children && <p>{children}</p>}
      {action}
    </div>
  );
}

export function Meter({ value, tone }: { value: number; tone?: 'warning' | 'critical' }) {
  return (
    <div className={['meter', tone && `meter--${tone}`].filter(Boolean).join(' ')} role="meter" aria-valuenow={Math.round(value * 100)} aria-valuemin={0} aria-valuemax={100}>
      <div className="meter__fill" style={{ width: `${Math.min(100, value * 100)}%` }} />
    </div>
  );
}

export function CodeBlock({ children }: { children: string }) {
  return <pre className="code">{children}</pre>;
}

export function Callout({ icon, children }: { icon: ReactNode; children: ReactNode }) {
  return <div className="callout">{icon}<div>{children}</div></div>;
}

/**
 * Page section: groups related cards under one heading.
 * Pages are a stack of Sections ordered by importance (what's broken → primary metrics → breakdowns → secondary).
 */
export function Section({ title, subtitle, actions, children }: { title?: ReactNode; subtitle?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="section">
      {(title || actions) && (
        <div className="section__head">
          <div className="section__titles">
            {title && <h2>{title}</h2>}
            {subtitle && <span className="section__subtitle">{subtitle}</span>}
          </div>
          {actions && <div className="section__actions">{actions}</div>}
        </div>
      )}
      {children}
    </section>
  );
}
