import type { ReactNode } from 'react';
import { Link } from 'react-router';

/**
 * Safe markdown subset for model output: headings, paragraphs, lists, block quotes, fenced code, pipe tables and
 * inline code / bold / italic / links. Builds React elements only (never HTML strings). Dashboard paths (`/…`)
 * become router links, http(s) links open in a new tab, anything else stays plain text.
 */
export function Markdown({ text }: { text: string }) {
  return <div className="md">{blocks(text)}</div>;
}

type Node = ReactNode;

function blocks(src: string): Node[] {
  const lines = src.replace(/\r\n?/g, '\n').split('\n');
  const out: Node[] = [];
  let i = 0;
  let key = 0;
  const isFence = (l: string) => /^\s*```/.test(l);
  const isList = (l: string) => /^\s*([-*+]|\d+[.)])\s+/.test(l);
  const isHeading = (l: string) => /^#{1,6}\s+/.test(l);
  const isQuote = (l: string) => /^\s*>/.test(l);
  const isTableRow = (l: string) => /^\s*\|.*\|\s*$/.test(l);
  const isTableSep = (l: string) => /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/.test(l);

  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }
    if (isFence(line)) {
      const lang = line.trim().slice(3).trim();
      const body: string[] = [];
      i++;
      while (i < lines.length && !isFence(lines[i])) body.push(lines[i++]);
      i++; // closing fence (or end of a still-streaming block)
      out.push(<pre key={key++} className="md__code" data-lang={lang || undefined}><code>{body.join('\n')}</code></pre>);
      continue;
    }
    if (isHeading(line)) {
      const level = Math.min(/^#+/.exec(line)![0].length, 4);
      const content = inline(line.replace(/^#+\s+/, ''));
      out.push(level <= 2 ? <h3 key={key++} className="md__h">{content}</h3> : <h4 key={key++} className="md__h md__h--sub">{content}</h4>);
      i++;
      continue;
    }
    if (isTableRow(line) && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const cells = (l: string) => l.trim().replace(/^\|/, '').replace(/\|$/, '').split('|').map((c) => c.trim());
      const head = cells(line);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && isTableRow(lines[i])) rows.push(cells(lines[i++]));
      out.push(
        <div key={key++} className="table-wrap md__table">
          <table className="table">
            <thead><tr>{head.map((h, j) => <th key={j}>{inline(h)}</th>)}</tr></thead>
            <tbody>{rows.map((r, j) => <tr key={j}>{head.map((_, k) => <td key={k}>{inline(r[k] ?? '')}</td>)}</tr>)}</tbody>
          </table>
        </div>,
      );
      continue;
    }
    if (isList(line)) {
      const ordered = /^\s*\d/.test(line);
      const items: string[] = [];
      while (i < lines.length && (isList(lines[i]) || (lines[i].trim() && /^\s{2,}/.test(lines[i]) && items.length))) {
        if (isList(lines[i])) items.push(lines[i].replace(/^\s*([-*+]|\d+[.)])\s+/, ''));
        else items[items.length - 1] += ' ' + lines[i].trim();
        i++;
      }
      const lis = items.map((t, j) => <li key={j}>{inline(t)}</li>);
      out.push(ordered ? <ol key={key++} className="md__list">{lis}</ol> : <ul key={key++} className="md__list">{lis}</ul>);
      continue;
    }
    if (isQuote(line)) {
      const body: string[] = [];
      while (i < lines.length && isQuote(lines[i])) body.push(lines[i++].replace(/^\s*>\s?/, ''));
      out.push(<blockquote key={key++} className="md__quote">{blocks(body.join('\n'))}</blockquote>);
      continue;
    }
    const para: string[] = [];
    while (i < lines.length && lines[i].trim() && !isFence(lines[i]) && !isHeading(lines[i]) && !isList(lines[i]) && !isQuote(lines[i])
      && !(isTableRow(lines[i]) && i + 1 < lines.length && isTableSep(lines[i + 1]))) para.push(lines[i++]);
    out.push(<p key={key++} className="md__p">{para.flatMap((l, j) => j ? [<br key={`b${j}`} />, ...inline(l)] : inline(l))}</p>);
  }
  return out;
}

// `code` | **bold** | __bold__ | *italic* | _italic_ | [text](url)
const INLINE = /(`[^`]+`)|(\*\*[^*]+\*\*|__[^_]+__)|(\*[^*\s][^*]*\*|_[^_\s][^_]*_)|(\[[^\]]+\]\([^)\s]+\))/g;

function inline(s: string): Node[] {
  const out: Node[] = [];
  let last = 0;
  let k = 0;
  for (const m of s.matchAll(INLINE)) {
    const at = m.index ?? 0;
    if (at > last) out.push(s.slice(last, at));
    const t = m[0];
    if (m[1]) out.push(<code key={k++} className="md__inline-code">{t.slice(1, -1)}</code>);
    else if (m[2]) out.push(<strong key={k++}>{inline(t.slice(2, -2))}</strong>);
    else if (m[3]) out.push(<em key={k++}>{inline(t.slice(1, -1))}</em>);
    else {
      const [, label, href] = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(t) ?? [];
      out.push(<MdLink key={k++} href={href ?? ''}>{inline(label ?? t)}</MdLink>);
    }
    last = at + t.length;
  }
  if (last < s.length) out.push(s.slice(last));
  return out;
}

function MdLink({ href, children }: { href: string; children: ReactNode }) {
  if (href.startsWith('/') && !href.startsWith('//')) return <Link to={href}>{children}</Link>;
  if (/^https?:\/\//i.test(href)) return <a href={href} target="_blank" rel="noopener noreferrer">{children}</a>;
  return <>{children}</>;
}
