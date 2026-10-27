import { ATTEMPT_HEADER } from './attrs';
import { recordError } from './errors';

/** Spring Kafka's delivery attempt header (4-byte big-endian int or decimal). */
export const KAFKA_DELIVERY_ATTEMPT_HEADER = 'kafka_deliveryAttempt';
/** RabbitMQ quorum queue redelivery count (0 on first delivery). */
export const RABBIT_DELIVERY_COUNT_HEADER = 'x-delivery-count';

type Headers = Record<string, unknown> | undefined | null;

function isBytes(v: unknown): v is Uint8Array {
  return v instanceof Uint8Array;
}

/** One header value as text (Buffer / string / number; arrays → last element). */
export function headerText(v: unknown): string | undefined {
  if (v == null) return undefined;
  if (Array.isArray(v)) return v.length ? headerText(v[v.length - 1]) : undefined;
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'bigint') return String(v);
  if (isBytes(v)) return new TextDecoder().decode(v);
  return undefined;
}

/** Parses a non-negative decimal attempt; undefined when absent or invalid. */
export function parseAttempt(v: unknown): number | undefined {
  if (typeof v === 'number') return Number.isInteger(v) && v >= 0 ? v : undefined;
  const s = headerText(v)?.trim();
  if (!s || !/^\d+$/.test(s)) return undefined;
  const n = Number(s);
  return Number.isSafeInteger(n) ? n : undefined;
}

/** Spring Kafka `kafka_deliveryAttempt`: 4-byte big-endian int, or a decimal string. */
export function kafkaDeliveryAttempt(v: unknown): number | undefined {
  const raw = Array.isArray(v) ? v[v.length - 1] : v;
  if (isBytes(raw) && raw.length === 4 && !raw.every((b) => b >= 48 && b <= 57)) {
    return ((raw[0]! << 24) | (raw[1]! << 16) | (raw[2]! << 8) | raw[3]!) >>> 0;
  }
  return parseAttempt(raw);
}

function xDeathCount(v: unknown): number | undefined {
  if (!Array.isArray(v)) return undefined;
  let total = 0;
  for (const e of v) {
    const c = (e as { count?: unknown } | null)?.count;
    if (typeof c === 'number' && c > 0) total += c;
  }
  return total;
}

/**
 * Delivery attempt from message headers (1 = first delivery), same order as the JVM / Go SDKs:
 * `tt-attempt` → `kafka_deliveryAttempt` → RabbitMQ `x-delivery-count` + 1 → RabbitMQ `x-death` counts + 1.
 * Undefined when none is present. Never throws.
 */
export function attemptFromHeaders(headers: Headers): number | undefined {
  if (!headers || typeof headers !== 'object') return undefined;
  try {
    const tt = parseAttempt(headers[ATTEMPT_HEADER]);
    if (tt !== undefined && tt > 0) return tt;
    const kafka = kafkaDeliveryAttempt(headers[KAFKA_DELIVERY_ATTEMPT_HEADER]);
    if (kafka !== undefined && kafka > 0) return kafka;
    const count = parseAttempt(headers[RABBIT_DELIVERY_COUNT_HEADER]);
    if (count !== undefined) return count + 1;
    const deaths = xDeathCount(headers['x-death']);
    if (deaths !== undefined && deaths > 0) return deaths + 1;
  } catch (err) {
    recordError('attempt', err);
  }
  return undefined;
}

/**
 * Writes `tt-attempt=<attempt>` into a header object (kafkajs `message.headers`, amqplib `options.headers`, a plain
 * carrier). Creates the object when `headers` is undefined. Returns the headers. Never throws.
 */
export function setAttemptHeader<T extends Record<string, unknown>>(headers: T | undefined, attempt: number): T {
  const h = (headers ?? {}) as Record<string, unknown>;
  try {
    h[ATTEMPT_HEADER] = String(Math.max(1, Math.floor(attempt)));
  } catch (err) {
    recordError('attempt', err);
  }
  return h as T;
}

/** Attempt number for the next publish: current + 1 (current defaults to 1). */
export function nextAttemptAfter(current: number | undefined): number {
  return (current && current >= 1 ? current : 1) + 1;
}
