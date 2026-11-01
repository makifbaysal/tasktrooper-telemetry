/** The contextual /local-ai actions are offline mocks and do not use the paid quota. */
export function LocalAICost() {
  return <span className="muted" style={{ fontSize: 'var(--text-xs)' }} title="Local mock actions never call Meta or Baseten">
    Est. 0 credits · Mock allowance: unlimited
  </span>;
}
