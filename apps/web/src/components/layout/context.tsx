import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import type { TimeRange } from '@/data';
import { getEnvFilter, setEnvFilter, useData } from '@/data/remote';

interface Ctx {
  range: TimeRange;
  setRange: (r: TimeRange) => void;
  /** deployment environment filter; '' = all environments */
  env: string;
  setEnv: (e: string) => void;
  /** user motion preference; the OS "reduce motion" setting always wins */
  motion: 'full' | 'reduced';
  setMotion: (m: 'full' | 'reduced') => void;
  theme: 'system' | 'light' | 'dark';
  setTheme: (t: 'system' | 'light' | 'dark') => void;
  /** bumps when api data arrives (ADR-013) — consumers re-render */
  dataVersion: number;
}

const AppCtx = createContext<Ctx | null>(null);

function read(key: string): string | null {
  try { return localStorage.getItem(key); } catch { return null; }
}
function write(key: string, v: string | null) {
  try { v === null ? localStorage.removeItem(key) : localStorage.setItem(key, v); } catch { /* storage unavailable */ }
}

export function AppProvider({ children }: { children: ReactNode }) {
  const [range, setRange] = useState<TimeRange>('1h');
  const [env, setEnvState] = useState(getEnvFilter);
  const setEnv = (e: string) => { setEnvState(e); write('trooper-env', e || null); setEnvFilter(e); };
  const [motion, setMotion] = useState<Ctx['motion']>(() => (read('trooper-motion') === 'reduced' ? 'reduced' : 'full'));
  const [theme, setTheme] = useState<Ctx['theme']>(() => (read('trooper-theme') as Ctx['theme']) ?? 'system');
  const dataVersion = useData();

  useEffect(() => {
    const root = document.documentElement;
    if (theme === 'system') { delete root.dataset.theme; write('trooper-theme', null); }
    else { root.dataset.theme = theme; write('trooper-theme', theme); }
  }, [theme]);

  useEffect(() => {
    const root = document.documentElement;
    if (motion === 'reduced') { root.dataset.motion = 'reduced'; write('trooper-motion', 'reduced'); }
    else { delete root.dataset.motion; write('trooper-motion', null); }
  }, [motion]);

  return <AppCtx.Provider value={{ range, setRange, env, setEnv, motion, setMotion, theme, setTheme, dataVersion }}>{children}</AppCtx.Provider>;
}

export function useApp() {
  const c = useContext(AppCtx);
  if (!c) throw new Error('useApp outside AppProvider');
  return c;
}
