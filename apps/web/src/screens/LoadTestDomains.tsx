import { useState } from 'react';
import { CircleDashed, Globe2, Plus, RefreshCw, ShieldCheck, Trash2 } from 'lucide-react';
import { Badge, Button, Card, CopyCode, EmptyState, Field, Input, PageHeader, Section, StatusBadge } from '@/components/ui';
import { isApi, useData } from '@/data';
import { addDomain, deleteDomain, domains, domainsAvailable, verifyDomain, type LoadTestDomain } from '@/data/loadtest';
import { ago } from '@/lib/format';

/**
 * Verified target domains (ADR-015): load can only be sent to hosts the project proved it owns —
 * a DNS TXT record `trooper-verify=<token>` or the token served at `/.well-known/trooper-verify`.
 */
export function LoadTestDomains() {
  useData();
  const [host, setHost] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [open, setOpen] = useState<string | null>(null);

  const add = async () => {
    setBusy(true); setError('');
    try { const d = await addDomain(host); setHost(''); setOpen(d.id); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };

  const pending = domains.filter((d) => !d.verifiedAt);
  const verified = domains.filter((d) => d.verifiedAt);

  return (
    <>
      <PageHeader title="Domains" crumbs={[{ label: 'Load tests', to: '/load-tests' }, { label: 'Domains' }]}
        subtitle="Load tests only run against hosts this project has verified. localhost and 127.0.0.1 are always allowed in development." />

      {isApi && !domainsAvailable ? (
        <Card>
          <EmptyState icon={<Globe2 size={20} />} title="Domain verification is not available yet">
            This api server has no <span className="mono">/loadtests/domains</span> endpoint yet. Runs against localhost still work.
          </EmptyState>
        </Card>
      ) : (
        <>
          <Card title="Add a domain">
            <form className="row" style={{ gap: 'var(--space-2)', alignItems: 'flex-end', flexWrap: 'wrap' }} onSubmit={(e) => { e.preventDefault(); void add(); }}>
              <div style={{ flex: 1, minWidth: 240 }}>
                <Field label="Host" hint="exact host the scenarios target — subdomains are verified separately">
                  <Input mono value={host} placeholder="staging.shop.com" onChange={(e) => setHost(e.target.value)} />
                </Field>
              </div>
              <Button type="submit" variant="primary" icon={<Plus size={14} />} disabled={!host.trim() || busy}>{busy ? 'Adding…' : 'Add domain'}</Button>
            </form>
            {error && <div className="row" style={{ gap: 'var(--space-2)', marginTop: 'var(--space-3)' }}><StatusBadge status="critical" label="Error" /><span className="secondary">{error}</span></div>}
          </Card>

          {pending.length > 0 && (
            <Section title="Waiting for verification" subtitle="Publish the token one of two ways, then verify">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                {pending.map((d) => <PendingDomain key={d.id} d={d} expanded={open === d.id || pending.length === 1 || open === null} />)}
              </div>
            </Section>
          )}

          <Section title="Verified">
            <Card flush>
              {verified.length === 0
                ? <EmptyState icon={<ShieldCheck size={20} />} title="No verified domain yet">Add the host your staging environment runs on.</EmptyState>
                : <div className="list">{verified.map((d) => <VerifiedRow key={d.id} d={d} />)}</div>}
            </Card>
          </Section>
        </>
      )}
    </>
  );
}

function VerifiedRow({ d }: { d: LoadTestDomain }) {
  const [confirm, setConfirm] = useState(false);
  return (
    <div className="list__row">
      <Globe2 size={16} className="muted" aria-hidden />
      <div className="list__main">
        <span className="list__title mono">{d.host}</span>
        <span className="list__meta">Verified {d.verifiedAt ? ago(d.verifiedAt) : ''}{d.method ? ` via ${d.method === 'dns' ? 'DNS TXT record' : 'well-known file'}` : ''}</span>
      </div>
      <StatusBadge status="ok" label="Verified" />
      {confirm
        ? <><Button size="sm" variant="danger" onClick={() => void deleteDomain(d.id)}>Remove</Button><Button size="sm" variant="ghost" onClick={() => setConfirm(false)}>Cancel</Button></>
        : <Button size="sm" variant="ghost" iconOnly aria-label={`Remove ${d.host}`} icon={<Trash2 size={14} />} onClick={() => setConfirm(true)} />}
    </div>
  );
}

function PendingDomain({ d, expanded }: { d: LoadTestDomain; expanded: boolean }) {
  const [busy, setBusy] = useState(false);
  const [show, setShow] = useState(expanded);
  const verify = async () => { setBusy(true); await verifyDomain(d.id).catch(() => undefined); setBusy(false); };
  return (
    <Card>
      <div className="stack" style={{ gap: 'var(--space-4)' }}>
        <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <Globe2 size={16} className="muted" aria-hidden />
          <span className="mono" style={{ fontWeight: 'var(--weight-semibold)' }}>{d.host}</span>
          {d.verifying || busy ? <Badge tone="accent" icon={<CircleDashed />}>Checking…</Badge> : d.lastError ? <StatusBadge status="warning" label="Not verified" /> : <Badge icon={<CircleDashed />}>Pending</Badge>}
          <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>added {ago(d.createdAt || Date.now())}</span>
          <span className="spacer" />
          <Button size="sm" variant="ghost" onClick={() => setShow((s) => !s)}>{show ? 'Hide instructions' : 'Show instructions'}</Button>
          <Button size="sm" variant="ghost" iconOnly aria-label={`Remove ${d.host}`} icon={<Trash2 size={14} />} onClick={() => void deleteDomain(d.id)} />
          <Button size="sm" variant="primary" icon={<RefreshCw size={14} />} disabled={busy} onClick={() => void verify()}>{busy ? 'Checking…' : 'Verify'}</Button>
        </div>
        {d.lastError && <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{d.lastError}</div>}
        {show && (
          <div className="grid grid-2" style={{ gap: 'var(--space-4)' }}>
            <div className="stack" style={{ gap: 'var(--space-2)' }}>
              <div className="eyebrow">Option 1 · DNS TXT record</div>
              <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>Add a TXT record on <span className="mono">{d.host}</span>:</span>
              <CopyCode label="Copy record name" wrap>{d.host}</CopyCode>
              <CopyCode label="Copy record value" wrap>{d.dnsRecord || `trooper-verify=${d.token}`}</CopyCode>
              <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>DNS changes can take a few minutes to propagate.</span>
            </div>
            <div className="stack" style={{ gap: 'var(--space-2)' }}>
              <div className="eyebrow">Option 2 · well-known file</div>
              <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>Serve the token as plain text at:</span>
              <CopyCode label="Copy URL" wrap>{d.httpUrl || `https://${d.host}/.well-known/trooper-verify`}</CopyCode>
              <CopyCode label="Copy token" wrap>{d.token}</CopyCode>
              <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>The response body must be exactly the token (whitespace is trimmed).</span>
            </div>
          </div>
        )}
      </div>
    </Card>
  );
}
