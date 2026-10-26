import { byteLength, MemoryOfflineStore, OFFLINE_MAX_BYTES, reportInternal, type OfflineStore } from '@trooper/core';

const req = <T>(r: IDBRequest<T>): Promise<T> =>
  new Promise((res, rej) => {
    r.onsuccess = () => res(r.result);
    r.onerror = () => rej(r.error);
  });

interface Row {
  p: string;
  n: number;
}

/**
 * Offline queue in IndexedDB (survives reloads), ≤ 5 MB, oldest dropped first.
 * Falls back to memory when IndexedDB is unavailable (private mode, old browsers, SSR).
 */
export class IdbOfflineStore implements OfflineStore {
  private db: Promise<IDBDatabase | undefined>;
  private readonly mem = new MemoryOfflineStore();
  private bytes = 0;

  constructor(
    private readonly name = 'trooper-rum',
    private readonly maxBytes = OFFLINE_MAX_BYTES,
  ) {
    this.db = this.open().catch((e) => {
      reportInternal('idb.open', e);
      return undefined;
    });
  }

  private async open(): Promise<IDBDatabase | undefined> {
    if (typeof indexedDB === 'undefined') return undefined;
    const r = indexedDB.open(this.name, 1);
    r.onupgradeneeded = () => r.result.createObjectStore('q', { autoIncrement: true });
    const db = await req(r);
    const rows = await req(db.transaction('q').objectStore('q').getAll() as IDBRequest<Row[]>);
    this.bytes = rows.reduce((n, x) => n + (x.n || 0), 0);
    return db;
  }

  private store(db: IDBDatabase, mode: IDBTransactionMode): IDBObjectStore {
    return db.transaction('q', mode).objectStore('q');
  }

  private async first(db: IDBDatabase): Promise<{ key: IDBValidKey; row: Row } | undefined> {
    const c = await req(this.store(db, 'readonly').openCursor());
    return c ? { key: c.key, row: c.value as Row } : undefined;
  }

  async push(p: string): Promise<void> {
    const db = await this.db;
    if (!db) return this.mem.push(p);
    try {
      const n = byteLength(p);
      if (n > this.maxBytes) return;
      await req(this.store(db, 'readwrite').add({ p, n } satisfies Row));
      this.bytes += n;
      while (this.bytes > this.maxBytes) {
        const f = await this.first(db);
        if (!f) break;
        await req(this.store(db, 'readwrite').delete(f.key));
        this.bytes -= f.row.n;
      }
    } catch (e) {
      reportInternal('idb.push', e);
      await this.mem.push(p);
    }
  }

  async peek(): Promise<string | undefined> {
    const db = await this.db;
    const m = await this.mem.peek();
    if (m !== undefined || !db) return m;
    try {
      return (await this.first(db))?.row.p;
    } catch (e) {
      reportInternal('idb.peek', e);
      return undefined;
    }
  }

  async shift(): Promise<void> {
    const db = await this.db;
    if ((await this.mem.size()) || !db) return this.mem.shift();
    try {
      const f = await this.first(db);
      if (f) {
        await req(this.store(db, 'readwrite').delete(f.key));
        this.bytes -= f.row.n;
      }
    } catch (e) {
      reportInternal('idb.shift', e);
    }
  }

  async size(): Promise<number> {
    const db = await this.db;
    const m = await this.mem.size();
    if (!db) return m;
    try {
      return m + (await req(this.store(db, 'readonly').count()));
    } catch {
      return m;
    }
  }
}
