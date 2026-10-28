import 'dart:async';
import 'dart:math';

import '../transport.dart';

/// Offline store of encoded segments (one entry per segment). Unlike the RUM queue, a segment that would push the
/// store past its cap is **dropped** (replay is optional data; the disk stays small). Never throws.
abstract class ReplaySegmentStore {
  /// Returns false when the segment was dropped (store full).
  Future<bool> add(List<int> bytes);

  /// Oldest stored segment, or null.
  Future<StoredSegment?> oldest();
  Future<void> remove(StoredSegment segment);
  Future<int> count();
}

class StoredSegment {
  StoredSegment(this.id, this.bytes);
  final String id;
  final List<int> bytes;
}

/// In-memory store (tests, fallback when the file system is unavailable).
class MemorySegmentStore implements ReplaySegmentStore {
  MemorySegmentStore({this.maxBytes = 2 * 1024 * 1024});
  final int maxBytes;
  final List<StoredSegment> _items = [];
  int _bytes = 0;
  int _seq = 0;
  int dropped = 0;

  @override
  Future<bool> add(List<int> bytes) async {
    if (_bytes + bytes.length > maxBytes) {
      dropped++;
      return false;
    }
    _items.add(StoredSegment('${_seq++}', bytes));
    _bytes += bytes.length;
    return true;
  }

  @override
  Future<StoredSegment?> oldest() async => _items.firstOrNull;

  @override
  Future<void> remove(StoredSegment segment) async {
    if (_items.remove(segment)) _bytes -= segment.bytes.length;
  }

  @override
  Future<int> count() async => _items.length;
}

/// Uploads stored segments oldest-first to `POST /v1/replay`; 429/5xx/network errors back off 1 s → 60 s with
/// jitter (`Retry-After` honoured), other 4xx (incl. 413) drop the segment (contracts.md §2, §23).
class ReplayUploader {
  ReplayUploader({
    required this.store,
    required this.transport,
    required this.url,
    required this.gzipped,
    Random? random,
  }) : _random = random ?? Random();

  final ReplaySegmentStore store;
  final RumTransport transport;
  final Uri url;
  final bool gzipped;
  final Random _random;

  bool _draining = false;
  bool _closed = false;
  Timer? _retry;
  int _attempt = 0;

  /// Segments uploaded / dropped by the server (internal counters).
  int sent = 0;
  int rejected = 0;

  bool get isBackingOff => _retry != null;

  Future<void> enqueue(List<int> bytes) async {
    try {
      await store.add(bytes);
      await drain();
    } catch (_) {}
  }

  Future<void> drain() async {
    if (_draining || _retry != null || _closed) return;
    _draining = true;
    try {
      while (!_closed) {
        final seg = await store.oldest();
        if (seg == null) return;
        final result = await transport.sendBytes(url, seg.bytes, gzipped: gzipped);
        if (result is SendRetry) {
          final delay = result.retryAfter ?? backoffDelay(_attempt, _random);
          _attempt++;
          _retry = Timer(delay, () {
            _retry = null;
            unawaited(drain());
          });
          return;
        }
        _attempt = 0;
        if (result is SendOk) {
          sent++;
        } else {
          rejected++;
        }
        await store.remove(seg);
      }
    } catch (_) {
    } finally {
      _draining = false;
    }
  }

  void close() {
    _closed = true;
    _retry?.cancel();
    _retry = null;
  }
}
