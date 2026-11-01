import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:meta/meta.dart';

import 'model.dart';
import 'storage.dart';
import 'transport.dart';

/// Batching + persistent FIFO queue + retrying uploader.
///
/// Events are buffered and cut into batches of ≤ [maxBatchSize] events,
/// either when the buffer is full or [flushInterval] after the first buffered
/// event. Serialized batches go to a queue persisted via [TrooperStorage]
/// (capped at [maxQueueBytes], oldest dropped first) and are uploaded one at a
/// time; retryable failures back off per contract (1 s → 60 s, jitter).
class RumPipeline {
  RumPipeline({
    required this.transport,
    required this.storage,
    required this.resource,
    this.maxBatchSize = 100,
    this.flushInterval = const Duration(seconds: 5),
    this.maxQueueBytes = 5 * 1024 * 1024,
    Random? random,
    this.onInternalError,
  }) : _random = random ?? Random();

  final RumTransport transport;
  final TrooperStorage storage;
  final RumResource resource;
  final int maxBatchSize;
  final Duration flushInterval;
  final int maxQueueBytes;
  final void Function(Object error)? onInternalError;
  final Random _random;

  final List<Map<String, Object?>> _buffer = [];
  final List<String> _queue = [];
  int _queueBytes = 0;
  Timer? _flushTimer;
  Timer? _retryTimer;
  bool _sending = false;
  int _attempt = 0;
  Future<void> _persisting = Future.value();
  bool _closed = false;

  @visibleForTesting
  List<Map<String, Object?>> get bufferedEvents => List.unmodifiable(_buffer);

  @visibleForTesting
  List<String> get queuedBatches => List.unmodifiable(_queue);

  @visibleForTesting
  bool get isBackingOff => _retryTimer != null;

  /// Loads batches persisted by a previous run and starts uploading them.
  Future<void> restore() async {
    try {
      final stored = await storage.loadQueue();
      for (final b in stored.reversed) {
        _queue.insert(0, b);
        _queueBytes += utf8.encode(b).length;
      }
      _enforceCap();
      unawaited(_drain());
    } catch (e) {
      onInternalError?.call(e);
    }
  }

  void add(Map<String, Object?> event) {
    if (_closed) return;
    _buffer.add(event);
    if (_buffer.length >= maxBatchSize) {
      unawaited(flush());
    } else {
      _flushTimer ??= Timer(flushInterval, () => unawaited(flush()));
    }
  }

  /// Cuts buffered events into batches, persists them and uploads.
  Future<void> flush() async {
    try {
      _flushTimer?.cancel();
      _flushTimer = null;
      if (_buffer.isNotEmpty) {
        final res = resource.toJson();
        while (_buffer.isNotEmpty) {
          final n = min(maxBatchSize, _buffer.length);
          final events = _buffer.sublist(0, n);
          _buffer.removeRange(0, n);
          _enqueue(jsonEncode(rumBatchJson(res, events)));
        }
        _persist();
      }
      await _drain();
    } catch (e) {
      onInternalError?.call(e);
    }
  }

  void _enqueue(String batch) {
    _queue.add(batch);
    _queueBytes += utf8.encode(batch).length;
    _enforceCap();
  }

  void _enforceCap() {
    while (_queueBytes > maxQueueBytes && _queue.isNotEmpty) {
      final dropped = _queue.removeAt(0);
      _queueBytes -= utf8.encode(dropped).length;
    }
  }

  void _remove(String batch) {
    if (_queue.remove(batch)) _queueBytes -= utf8.encode(batch).length;
  }

  void _persist() {
    final snapshot = List.of(_queue);
    _persisting = _persisting.then((_) => storage.saveQueue(snapshot)).catchError((Object e) {
      onInternalError?.call(e);
    });
  }

  Future<void> _drain() async {
    if (_sending || _retryTimer != null || _closed) return;
    _sending = true;
    try {
      while (_queue.isNotEmpty && !_closed) {
        final batch = _queue.first;
        final result = await transport.send(batch);
        if (result is SendRetry) {
          final delay = result.retryAfter ?? backoffDelay(_attempt, _random);
          _attempt++;
          _retryTimer = Timer(delay, () {
            _retryTimer = null;
            unawaited(_drain());
          });
          return;
        }
        _attempt = 0;
        _remove(batch);
        _persist();
      }
    } catch (e) {
      onInternalError?.call(e);
    } finally {
      _sending = false;
    }
  }

  /// Persists the buffer and stops timers (pending batches stay on disk).
  Future<void> close() async {
    _flushTimer?.cancel();
    _flushTimer = null;
    if (_buffer.isNotEmpty) {
      final res = resource.toJson();
      while (_buffer.isNotEmpty) {
        final n = min(maxBatchSize, _buffer.length);
        _enqueue(jsonEncode(rumBatchJson(res, _buffer.sublist(0, n))));
        _buffer.removeRange(0, n);
      }
      _persist();
    }
    _closed = true;
    _retryTimer?.cancel();
    _retryTimer = null;
    await _persisting;
  }
}
