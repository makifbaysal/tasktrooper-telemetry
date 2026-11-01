import { useState } from 'react';
import { Link } from 'react-router';
import { AppWindow, ArrowRight, Check, Server, Smartphone } from 'lucide-react';
import { HeroScene } from '@/components/public/ServiceMap';
import { PublicFooter, PublicNav, useHashScroll } from '@/components/public/PublicNav';
import { CodeBlock } from '@/components/public/CodeBlock';
import { DOC_SDKS, INSTALL_CMD, INSTALL_TOOLS, SKILLS } from '@/components/public/docsContent';

/**
 * Public landing page (signed-out `/`). Explains the product; sign-in lives on its own page (`/login`), there is no
 * sign-up. Numbers on this page are illustrative (a fictional shop), never customer data. Surface brief:
 * .impeccable/surfaces/src-screens-landing-tsx.md
 */

const SIGNALS = [
  {
    name: 'APM',
    body: 'Every endpoint and queue consumer with throughput, error rate and p50, p95, p99. Traces show the full call tree; consumers show lag, retries and dead letters.',
    sees: ['POST /orders · p95 212 ms', 'payments.settled · lag 140'],
  },
  {
    name: 'Real-user monitoring',
    body: 'Page and screen views, Web Vitals and app start, JS errors and crashes, network calls and session replay, for web, iOS, Android, React Native and Flutter.',
    sees: ['/checkout · LCP 1.9 s', 'courier-app · crash-free 99.6%'],
  },
  {
    name: 'Logs and alerting',
    body: 'Log search linked to traces. KPIs with error-budget burn-rate alerts, plus metric, log-pattern and anomaly monitors, delivered to Slack with the cause attached.',
    sees: ['Homepage latency · 14× burn', 'level:error trace:4bf9…'],
  },
  {
    name: 'Load testing',
    body: 'Generate a scenario from real production traffic, run it against a verified staging host, and read the results next to the same telemetry.',
    sees: ['checkout-mix · 400 rps', 'p95 under 300 ms · passed'],
  },
];

const SURFACES = [
  {
    name: 'Backend',
    icon: Server,
    what: 'HTTP services, workers and queue consumers',
    sees: [
      'Throughput, errors and p50/p95/p99 per route',
      'Traces across services, databases and caches',
      'Consumer lag, retries and dead letters',
      'Continuous profiling and flame graphs',
    ],
    sdks: 'Go · Java and Kotlin · .NET · Python · Node.js',
  },
  {
    name: 'Web',
    icon: AppWindow,
    what: 'Single-page apps and websites',
    sees: [
      'Page views, visitors and sessions',
      'Web Vitals: LCP, INP, CLS, TTFB',
      'JS errors with source maps',
      'Network calls joined to backend traces, session replay',
    ],
    sdks: 'Browser · React · Vue · Angular',
  },
  {
    name: 'Mobile',
    icon: Smartphone,
    what: 'iOS, Android and cross-platform apps',
    sees: [
      'Screens, sessions, cold and warm start',
      'Crashes (native included) with symbolication',
      'Slow frames and API calls joined to backend traces',
      'Session replay',
    ],
    sdks: 'iOS · Android · React Native · Flutter',
  },
];

const PROPOSED = [
  { route: 'POST /orders', rule: 'KPI 99.5% availability over 30 days', why: 'Critical path' },
  { route: 'GET /orders/:id', rule: 'p95 latency under 300 ms', why: 'Highest traffic' },
  { route: 'payments.settled', rule: 'Consumer lag under 1,000 messages', why: 'Queue consumer' },
];

const TIMELINE = [
  { t: '14:02', title: 'Deploy', body: 'reco-db migration 0042 ships with recommendation-api 2.14.' },
  { t: '14:06', title: 'Latency rises', body: 'Homepage p95 climbs from 380 ms to 4.1 s for real users.' },
  { t: '14:09', title: 'Alert', body: 'The Homepage latency KPI burns its error budget 14× too fast. Slack gets the alert.' },
  { t: '14:09', title: 'Cause', body: 'Root-cause analysis walks the call tree: reco-db queries are 63× slower since the migration.' },
  { t: '14:31', title: 'Resolved', body: 'Migration rolled back, p95 back to 380 ms, the incident closes itself.' },
];

const TAB_SDKS = ['go', 'jvm', 'dotnet', 'python', 'node', 'browser'];

export function Landing() {
  useHashScroll();
  const [sdk, setSdk] = useState('go');
  const current = DOC_SDKS.find((s) => s.id === sdk) ?? DOC_SDKS[0];

  return (
    <div className="public">
      <section className="hero" aria-labelledby="hero-title">
        <PublicNav tone="field" />
        <div className="hero__grid">
          <div className="hero__copy">
            <h1 id="hero-title">Know why it’s slow, not just that it is.</h1>
            <p className="hero__lede">
              One place for your backend services, web apps and mobile apps: APM, real-user monitoring, logs, alerts and
              load tests. When something breaks, TaskTrooper follows the request from the user’s screen to the database
              and names the cause.
            </p>
            <div className="hero__actions">
              <Link to={{ hash: 'product' }} className="pbtn pbtn--light">See how it works</Link>
              <Link to="/docs" className="pbtn pbtn--outline">Read the docs</Link>
            </div>
          </div>
          <HeroScene />
        </div>
      </section>

      <main>
        <section id="product" className="psec" aria-labelledby="product-title">
          <div className="psec__head">
            <h2 id="product-title">Backend, web and mobile, read as one system.</h2>
            <p>Add one SDK per service or app. A tap in the mobile app or a click on the website joins the backend trace it
              caused, so a slow screen and the slow query behind it are one investigation, not three tools.</p>
          </div>
          <div className="surfaces">
            {SURFACES.map((s) => (
              <section key={s.name} className="surfaces__col" aria-labelledby={`surface-${s.name}`}>
                <header className="surfaces__head">
                  <span className="surfaces__icon"><s.icon size={20} aria-hidden /></span>
                  <div>
                    <h3 id={`surface-${s.name}`}>{s.name}</h3>
                    <p>{s.what}</p>
                  </div>
                </header>
                <ul className="surfaces__list">
                  {s.sees.map((x) => <li key={x}><Check size={14} aria-hidden />{x}</li>)}
                </ul>
                <p className="surfaces__sdks">{s.sdks}</p>
              </section>
            ))}
          </div>
          <h3 className="signals__title">Signals across all three</h3>
          <dl className="signals">
            {SIGNALS.map((s) => (
              <div key={s.name} className="signals__row">
                <dt>{s.name}</dt>
                <dd className="signals__body">{s.body}</dd>
                <dd className="signals__sees">{s.sees.map((x) => <span key={x}>{x}</span>)}</dd>
              </div>
            ))}
          </dl>
          <p className="psec__aside">Also in the box: continuous profiling with flame graphs, host and Kubernetes metrics,
            serverless functions, and LLM calls with token cost.</p>
        </section>

        <section className="band" aria-labelledby="code-title">
          <div className="band__inner">
            <div className="psec__head psec__head--field">
              <h2 id="code-title">Connect a repo. Get the alerts you would have written after the outage.</h2>
              <p>TaskTrooper reads your routes, consumers and jobs, matches them with production traffic, and proposes a KPI and
                its monitors for each critical path. You accept, edit or dismiss.</p>
            </div>
            <div className="c2m">
              <div className="c2m__code" aria-label="orders-api/internal/http/routes.go">
                <div className="c2m__file">orders-api / internal/http/routes.go</div>
                <pre>
                  <span>r := chi.NewRouter()</span>
                  <span className="is-hit">r.Post("/orders", h.CreateOrder)</span>
                  <span className="is-hit">r.Get("/orders/{'{id}'}", h.GetOrder)</span>
                  <span>r.Get("/healthz", h.Health)</span>
                  <span className="is-hit">consumer.Handle("payments.settled", h.Settle)</span>
                </pre>
              </div>
              <ArrowRight className="c2m__arrow" size={20} aria-hidden />
              <div className="c2m__out">
                <div className="c2m__file">Proposed for orders-api</div>
                <ul>
                  {PROPOSED.map((p) => (
                    <li key={p.route}>
                      <span className="c2m__route">{p.route}</span>
                      <span className="c2m__rule">{p.rule}</span>
                      <span className="c2m__why">{p.why}</span>
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          </div>
        </section>

        <section className="psec" aria-labelledby="cause-title">
          <div className="psec__head">
            <h2 id="cause-title">From symptom to cause in minutes, not a war room.</h2>
            <p>Alerts fire on what users feel. The incident arrives with the dependency, the deploy and the evidence that
              point to the cause, or it says “unknown” when the evidence is thin.</p>
          </div>
          <ol className="timeline">
            {TIMELINE.map((s) => (
              <li key={s.title}>
                <time>{s.t}</time>
                <strong>{s.title}</strong>
                <p>{s.body}</p>
              </li>
            ))}
          </ol>
        </section>

        <section id="sdks" className="psec" aria-labelledby="sdks-title">
          <div className="psec__head">
            <h2 id="sdks-title">Install one SDK. Your service shows up with the first request.</h2>
            <p>Open-source SDKs on npm, PyPI, Maven Central, NuGet, pub.dev, CocoaPods and Go modules. They never crash the
              host app, batch in the background and collect no headers, bodies or query strings by default.</p>
          </div>
          <div className="sdks">
            <div className="sdks__main">
              <div className="ptabs" role="tablist" aria-label="SDK">
                {TAB_SDKS.map((id) => {
                  const s = DOC_SDKS.find((x) => x.id === id)!;
                  return (
                    <button key={id} type="button" role="tab" aria-selected={sdk === id} className="ptabs__tab" onClick={() => setSdk(id)}>{s.label}</button>
                  );
                })}
              </div>
              <div role="tabpanel" className="sdks__panel">
                <CodeBlock caption={`Install · ${current.label}`} code={current.install.code} />
                <CodeBlock caption="Initialise" code={current.init.code} />
              </div>
            </div>
            <div className="sdks__all">
              <h3>All platforms</h3>
              {(['Backend', 'Web', 'Mobile'] as const).map((g) => (
                <div key={g} className="sdks__group">
                  <span className="sdks__group-name">{g}</span>
                  <span>
                    {DOC_SDKS.filter((s) => s.group === g).map((s, i, a) => (
                      <span key={s.id}><Link to={`/docs#${s.id}`}>{s.label}</Link>{i < a.length - 1 ? ', ' : ''}</span>
                    ))}
                  </span>
                </div>
              ))}
              <Link to="/docs" className="plink">Setup guide <ArrowRight size={14} aria-hidden /></Link>
            </div>
          </div>
        </section>

        <section className="psec" aria-labelledby="agents-title">
          <div className="psec__head">
            <h2 id="agents-title">Ask your coding agent, with evidence.</h2>
            <p>An MCP server gives your coding agent the same services, traces, logs, incidents and deploys you see. Agent
              skills tell it how to investigate and how to set up alerts and SLOs from your own traffic, with your approval
              for every change. One command installs both in {INSTALL_TOOLS.slice(0, -1).join(', ')} or {INSTALL_TOOLS.at(-1)}.</p>
          </div>
          <div className="agents">
            <div className="agents__setup">
              <CodeBlock caption="Install for your coding agents" code={INSTALL_CMD} />
              <p className="agents__hint">Finds the tools on your machine, asks which ones and whether globally or for one project.</p>
              <Link to="/docs#install" className="plink">Install guide, MCP server and skills <ArrowRight size={14} aria-hidden /></Link>
            </div>
            <dl className="agents__skills">
              {SKILLS.filter((s) => s.needsMcp).map((s) => (
                <div key={s.name}>
                  <dt>{s.name}</dt>
                  <dd>{s.when}</dd>
                </div>
              ))}
            </dl>
          </div>
        </section>

        <section className="close" aria-labelledby="close-title">
          <h2 id="close-title">Already on a team that uses TaskTrooper?</h2>
          <p>Sign in with your company account. New here? Start with the docs and connect your first service.</p>
          <div className="hero__actions">
            <Link to="/login" className="pbtn pbtn--solid">Sign in</Link>
            <Link to="/docs" className="pbtn pbtn--ghost">Read the docs</Link>
          </div>
        </section>
      </main>

      <PublicFooter />
    </div>
  );
}
