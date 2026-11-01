import { useEffect, useState } from 'react';
import { Link, useLocation } from 'react-router';
import { ExternalLink } from 'lucide-react';
import { PublicFooter, PublicNav, useHashScroll } from '@/components/public/PublicNav';
import { CodeBlock } from '@/components/public/CodeBlock';
import {
  DOC_GUIDES, DOC_SDKS, DSN_PLACEHOLDER, INSTALL_CMD, INSTALL_FLAGS, INSTALL_STEPS, INSTALL_TOOLS, MCP_NOTES, MCP_SETUP, MCP_TOOLS, MCP_WRITE_TOOLS, REPO, SITE, SITE_HOST, SKILLS, SKILLS_SETUP,
} from '@/components/public/docsContent';

/**
 * Public docs (`/docs`, signed in or not): getting started, SDK install per platform, the MCP server and agent
 * skills, and short guides. Facts only; each SDK links to its README for the full reference.
 */

const RAIL: { title: string; items: { id: string; label: string }[] }[] = [
  { title: 'Start', items: [{ id: 'getting-started', label: 'Getting started' }] },
  { title: 'SDKs', items: DOC_SDKS.map((s) => ({ id: s.id, label: s.label })) },
  { title: 'AI agents', items: [{ id: 'install', label: 'One-command install' }, { id: 'mcp', label: 'MCP server' }, { id: 'skills', label: 'Agent skills' }] },
  { title: 'Guides', items: DOC_GUIDES.map((g) => ({ id: g.id, label: g.title })) },
];
const ALL_IDS = RAIL.flatMap((g) => g.items.map((i) => i.id));

/** Highlights the rail entry of the section nearest the top of the viewport. */
function useActiveSection(): string {
  const { hash } = useLocation();
  const [active, setActive] = useState(hash.slice(1) || ALL_IDS[0]);
  useEffect(() => {
    const els = ALL_IDS.map((id) => document.getElementById(id)).filter((e): e is HTMLElement => !!e);
    const io = new IntersectionObserver((entries) => {
      const top = entries.filter((e) => e.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top)[0];
      if (top) setActive(top.target.id);
    }, { rootMargin: '-80px 0px -65% 0px' });
    els.forEach((e) => io.observe(e));
    return () => io.disconnect();
  }, []);
  return active;
}

const Readme = ({ path }: { path: string }) => (
  <a className="plink" href={`${REPO}/tree/main/${path}#readme`} target="_blank" rel="noreferrer">
    Full reference on GitHub <ExternalLink size={13} aria-hidden />
  </a>
);

/** Inline `code` spans written with backticks in docsContent strings. */
const md = (text: string) => text.split(/(`[^`]+`)/).map((part, i) => (part.startsWith('`') ? <code key={i}>{part.slice(1, -1)}</code> : part));

export function Docs() {
  useHashScroll();
  const active = useActiveSection();

  return (
    <div className="public public--docs">
      <PublicNav />
      <div className="docs">
        <nav className="docs__rail" aria-label="Documentation">
          {RAIL.map((g) => (
            <div key={g.title} className="docs__rail-group">
              <span className="docs__rail-title">{g.title}</span>
              {g.items.map((i) => (
                <Link key={i.id} to={{ hash: i.id }} aria-current={active === i.id ? 'location' : undefined}>{i.label}</Link>
              ))}
            </div>
          ))}
        </nav>

        <main className="docs__body">
          <header className="docs__intro">
            <h1>Documentation</h1>
            <p>Add an SDK, set a DSN, deploy. Your service appears in the dashboard with its first request, and alerts,
              root cause and load tests build on the same data.</p>
          </header>

          <section id="getting-started" className="docs__sec">
            <h2>Getting started</h2>
            <ol className="docs__steps">
              <li><strong>Sign in</strong> at <a href={`${SITE}/login`}>{SITE_HOST}</a> with your company account (SSO). The first person creates the organisation and its first project.</li>
              <li><strong>Create an ingest key.</strong> Settings → API keys. The project’s DSN carries it: <code>{DSN_PLACEHOLDER}</code></li>
              <li><strong>Install an SDK</strong> in each service or app and pass the DSN, usually through <code>TROOPER_DSN</code>.</li>
              <li><strong>Deploy.</strong> Traces, metrics and logs travel over OTLP. The service, its endpoints and the deploy marker appear with the first request.</li>
              <li><strong>Connect a repository</strong> (optional) to get KPI and monitor suggestions from your code.</li>
            </ol>
          </section>

          {DOC_SDKS.map((s) => (
            <section key={s.id} id={s.id} className="docs__sec">
              <h2>{s.label}</h2>
              <p>{s.summary}</p>
              <CodeBlock caption="Install" code={s.install.code} />
              <CodeBlock caption="Initialise" code={s.init.code} />
              {s.notes?.map((n) => <p key={n} className="docs__note">{md(n)}</p>)}
              <Readme path={s.readme} />
            </section>
          ))}

          <section id="install" className="docs__sec">
            <h2>One-command install</h2>
            <p>Sets up the MCP server and the agent skills in {INSTALL_TOOLS.slice(0, -1).join(', ')} and {INSTALL_TOOLS.at(-1)}.
              Run it in a terminal on macOS or Linux (Windows: WSL); from a project folder if you want a project install.</p>
            <CodeBlock caption="Terminal" code={INSTALL_CMD} />
            <ol className="docs__list">{INSTALL_STEPS.map((s) => <li key={s}>{s}</li>)}</ol>
            <CodeBlock caption={INSTALL_FLAGS.label} code={INSTALL_FLAGS.code} />
            <p className="docs__note">Run it again to add a tool, change the key or turn writes on or off. Manual setup per tool is below.</p>
          </section>

          <section id="mcp" className="docs__sec">
            <h2>MCP server</h2>
            <p><code>tt-mcp</code> is a Model Context Protocol server for coding agents such as Claude Code, Muse Code, Cursor,
              Antigravity and OpenCode. It runs locally over stdio, wraps the TaskTrooper API and is scoped to one project
              by its API key. It only reads unless you turn writes on.</p>
            {MCP_SETUP.map((c) => <CodeBlock key={c.label} caption={c.label} code={c.code} />)}
            <ul className="docs__list">{MCP_NOTES.map((n) => <li key={n}>{n}</li>)}</ul>
            <h3>Tools</h3>
            <p>Tools that read a time span accept <code>range</code> (<code>15m</code> to <code>7d</code>, default <code>1h</code>) and an optional <code>env</code>.</p>
            <dl className="docs__defs">
              {MCP_TOOLS.map((t) => (
                <div key={t.name}><dt><code>{t.name}</code></dt><dd>{t.what}</dd></div>
              ))}
            </dl>
            <h3>Write tools</h3>
            <p>Only with <code>TT_MCP_ALLOW_WRITE=true</code>. Each call first returns what it would send; the agent shows it to
              you and writes only after you approve.</p>
            <dl className="docs__defs">
              {MCP_WRITE_TOOLS.map((t) => (
                <div key={t.name}><dt><code>{t.name}</code></dt><dd>{t.what}</dd></div>
              ))}
            </dl>
            <Readme path="services/mcp" />
          </section>

          <section id="skills" className="docs__sec">
            <h2>Agent skills</h2>
            <p>Skills tell an agent when to use the tools and in which order. They live in <code>skills/</code> in the
              repository; each is a single <code>SKILL.md</code>.</p>
            <dl className="docs__defs">
              {SKILLS.map((s) => (
                <div key={s.name}>
                  <dt><code>{s.name}</code></dt>
                  <dd>{s.when}{s.needsMcp && <span className="docs__req"> Needs the MCP server.</span>}</dd>
                </div>
              ))}
            </dl>
            {SKILLS_SETUP.map((c) => <CodeBlock key={c.label} caption={c.label} code={c.code} />)}
            <Readme path="skills" />
          </section>

          {DOC_GUIDES.map((g) => (
            <section key={g.id} id={g.id} className="docs__sec">
              <h2>{g.title}</h2>
              {g.body.map((p) => <p key={p}>{p}</p>)}
            </section>
          ))}
        </main>
      </div>
      <PublicFooter />
    </div>
  );
}
