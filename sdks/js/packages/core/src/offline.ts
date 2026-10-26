import { byteLength } from './util';

/**
 * FIFO queue of serialized batches that could not be delivered yet (offline / 429 / 5xx).
 * Implementations must cap total size (default 5 MB) by dropping the OLDEST entries (contracts.md §9).
 */
export interface OfflineStore {
  push(payload: string): Promise<void>;
  peek(): Promise<string | undefined>;
  /** Remove the head (the value returned by the last `peek`). */
  shift(): Promise<void>;
  size(): Promise<number>;
}

export const OFFLINE_MAX_BYTES = 5 * 1024 * 1024;

export class MemoryOfflineStore implements OfflineStore {
  private items: { p: string; n: number }[] = [];
  private bytes = 0;

  constructor(private readonly maxBytes = OFFLINE_MAX_BYTES) {}

  push(payload: string): Promise<void> {
    const n = byteLength(payload);
    if (n <= this.maxBytes) {
      this.items.push({ p: payload, n });
      this.bytes += n;
      while (this.bytes > this.maxBytes) this.bytes -= this.items.shift()!.n;
    }
    return Promise.resolve();
  }

  peek(): Promise<string | undefined> {
    return Promise.resolve(this.items[0]?.p);
  }

  shift(): Promise<void> {
    const it = this.items.shift();
    if (it) this.bytes -= it.n;
    return Promise.resolve();
  }

  size(): Promise<number> {
    return Promise.resolve(this.items.length);
  }

  snapshot(): string[] {
    return this.items.map((i) => i.p);
  }

  /** Current byte usage (for tests / diagnostics). */
  byteSize(): number {
    return this.bytes;
  }
}

/** Minimal async key/value interface (AsyncStorage-compatible). */
export interface KeyValueStorage {
  getItem(key: string): Promise<string | null>;
  setItem(key: string, value: string): Promise<void>;
  removeItem?(key: string): Promise<void>;
}

/**
 * OfflineStore persisted as one JSON array under `key` in an async key/value storage
 * (React Native AsyncStorage). Writes are serialized; memory is the source of truth.
 */
export class KeyValueOfflineStore implements OfflineStore {
  private mem: MemoryOfflineStore;
  private ready: Promise<void>;
  private writing: Promise<void> = Promise.resolve();

  constructor(
    private readonly storage: KeyValueStorage,
    private readonly key = 'trooper:offline',
    maxBytes = OFFLINE_MAX_BYTES,
  ) {
    this.mem = new MemoryOfflineStore(maxBytes);
    this.ready = this.storage
      .getItem(this.key)
      .then(async (raw) => {
        const arr: unknown = raw ? JSON.parse(raw) : [];
        if (Array.isArray(arr)) for (const p of arr) if (typeof p === 'string') await this.mem.push(p);
      })
      .catch(() => undefined);
  }

  private persist(): Promise<void> {
    this.writing = this.writing
      .then(async () => {
        await this.storage.setItem(this.key, JSON.stringify(this.mem.snapshot()));
      })
      .catch(() => undefined);
    return this.writing;
  }

  async push(payload: string): Promise<void> {
    await this.ready;
    await this.mem.push(payload);
    await this.persist();
  }

  async peek(): Promise<string | undefined> {
    await this.ready;
    return this.mem.peek();
  }

  async shift(): Promise<void> {
    await this.ready;
    await this.mem.shift();
    await this.persist();
  }

  async size(): Promise<number> {
    await this.ready;
    return this.mem.size();
  }
}
