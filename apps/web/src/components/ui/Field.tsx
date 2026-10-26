import type { InputHTMLAttributes, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from 'react';
import { Search } from 'lucide-react';

export function Field({ label, hint, children }: { label: ReactNode; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="field">
      <span className="field__label">{label}</span>
      {children}
      {hint && <span className="field__hint">{hint}</span>}
    </label>
  );
}

export function Input({ mono, className, ...rest }: InputHTMLAttributes<HTMLInputElement> & { mono?: boolean }) {
  return <input className={['input', mono && 'input--mono', className].filter(Boolean).join(' ')} {...rest} />;
}

/** Multi-line input; `mono` for code-like content (JSON bodies, headers). */
export function Textarea({ mono, className, ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement> & { mono?: boolean }) {
  return <textarea className={['input', 'textarea', mono && 'input--mono', className].filter(Boolean).join(' ')} {...rest} />;
}

export function SearchInput(props: InputHTMLAttributes<HTMLInputElement> & { mono?: boolean }) {
  return (
    <div className="input-group" style={{ flex: 1 }}>
      <Search size={14} aria-hidden />
      <Input type="search" {...props} />
    </div>
  );
}

export function Select({ options, ...rest }: SelectHTMLAttributes<HTMLSelectElement> & { options: { value: string; label: string }[] }) {
  return (
    <select className="select" {...rest}>
      {options.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
    </select>
  );
}

export function Toggle({ checked, onChange, label }: { checked: boolean; onChange: (v: boolean) => void; label: string }) {
  return <button type="button" role="switch" aria-checked={checked} aria-label={label} className="toggle" onClick={() => onChange(!checked)} />;
}
