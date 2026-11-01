import { useState } from 'react';
import { Check, Copy } from 'lucide-react';

/** Code on the public pages: a caption row (file / language) with a copy action, then the snippet. */
export function CodeBlock({ code, caption }: { code: string; caption?: string }) {
  const [done, setDone] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(code).then(() => { setDone(true); setTimeout(() => setDone(false), 1500); }, () => undefined);
  };
  return (
    <figure className="pcode">
      <figcaption className="pcode__bar">
        <span>{caption}</span>
        <button type="button" className="pcode__copy" onClick={copy} aria-label={done ? 'Copied' : 'Copy code'}>
          {done ? <Check size={13} aria-hidden /> : <Copy size={13} aria-hidden />}
          {done ? 'Copied' : 'Copy'}
        </button>
      </figcaption>
      <pre><code>{code}</code></pre>
    </figure>
  );
}
