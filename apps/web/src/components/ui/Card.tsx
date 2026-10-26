import type { ReactNode } from 'react';

interface Props {
  title?: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  flush?: boolean;
  onClick?: () => void;
  className?: string;
  children?: ReactNode;
}

export function Card({ title, subtitle, actions, flush, onClick, className, children }: Props) {
  return (
    <section className={['card', onClick && 'card--interactive', className].filter(Boolean).join(' ')} onClick={onClick}>
      {(title || actions) && (
        <header className="card__header">
          <div>
            {title && <h3 className="card__title">{title}</h3>}
            {subtitle && <div className="card__subtitle">{subtitle}</div>}
          </div>
          {actions && <div className="card__actions">{actions}</div>}
        </header>
      )}
      <div className={flush ? 'card__body card__body--flush' : 'card__body'}>{children}</div>
    </section>
  );
}
