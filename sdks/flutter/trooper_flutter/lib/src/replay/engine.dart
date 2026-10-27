import 'dart:convert';
import 'dart:math';

import 'options.dart';
import 'wireframe.dart';

/// RUM context the replay engine follows (the Dart client owns the session, contracts.md §9).
class ReplayContext {
  const ReplayContext({
    required this.sessionId,
    required this.sampled,
    required this.anonymousId,
    this.viewId,
    this.viewName,
    this.userId,
  });
  final String sessionId;

  /// RUM sampling of the session: unsampled sessions are never recorded.
  final bool sampled;
  final String anonymousId;
  final String? viewId;
  final String? viewName;
  final String? userId;
}

/// `resource` block of a segment.
class ReplayResource {
  const ReplayResource({
    required this.service,
    required this.env,
    required this.version,
    required this.platform,
    required this.sdkVersion,
  });
  final String service;
  final String env;
  final String version;

  /// `android` | `ios` (contracts.md §23 hybrid apps).
  final String platform;
  final String sdkVersion;

  Map<String, Object?> toJson() => {
    'service': service,
    'env': env,
    'version': version,
    'platform': platform,
    'sdk_name': ReplayEngine.sdkName,
    'sdk_version': sdkVersion,
  };
}

/// Persisted per-session state: sampling decision and next segment number.
class ReplayState {
  const ReplayState(this.sessionId, this.segment, this.mode);
  final String sessionId;
  final int segment;
  final ReplayMode mode;

  String encode() => jsonEncode({'session_id': sessionId, 'next_segment': segment, 'mode': mode.name});

  static ReplayState? decode(String? s) {
    if (s == null || s.isEmpty) return null;
    try {
      final m = jsonDecode(s) as Map<String, Object?>;
      final mode = ReplayMode.values.where((v) => v.name == m['mode']).firstOrNull;
      final sid = m['session_id'];
      final seg = m['next_segment'];
      if (sid is! String || seg is! int || mode == null) return null;
      return ReplayState(sid, seg, mode);
    } catch (_) {
      return null;
    }
  }
}

enum ReplayMode { off, buffer, record }

class _Ev {
  _Ev(this.ts, this.json, this.type);
  final int ts;
  final String json;
  final int type;
}

class _Screen {
  const _Screen(this.width, this.height, this.density, this.name);
  final int width;
  final int height;
  final double density;
  final String? name;

  @override
  bool operator ==(Object other) =>
      other is _Screen &&
      other.width == width &&
      other.height == height &&
      other.density == density &&
      other.name == name;

  @override
  int get hashCode => Object.hash(width, height, density, name);
}

/// Sampling, on-error ring buffer and segmenting for wireframe replay (contracts.md §23). Pure Dart; same
/// behaviour as the native Android / iOS engines.
///
/// * Per RUM session: `sampleRate` → [ReplayMode.record]; else `onErrorSampleRate` → [ReplayMode.buffer] (last
///   60 s in memory; on ERROR the buffer is flushed and recording continues); else off. Fixed for the session and
///   persisted with the segment counter, so a restarted app continues the same session's numbering.
/// * A segment is sealed when it is ≥ 5 s old or ≥ 256 KB (uncompressed), on [flush] (app in background) and on
///   session change. Every segment starts with a `screen` event.
class ReplayEngine {
  ReplayEngine({
    required this.options,
    required this.now,
    required this.context,
    required this.resource,
    required this.sink,
    this.persist,
    ReplayState? restored,
    Random? random,
  }) : _restored = restored,
       _random = random ?? Random();

  static const sdkName = 'trooper_flutter';
  static const segmentMaxMs = 5000;
  static const segmentMaxBytes = 256 * 1024;
  static const bufferMs = 60000;
  static const bufferMaxBytes = 4 * 1024 * 1024;

  static const _screen = 1;
  static const _frame = 2;
  static const _touch = 3;
  static const _keyboard = 4;

  final TrooperReplayOptions options;
  final int Function() now;
  final ReplayContext? Function() context;
  final ReplayResource Function() resource;

  /// Receives each sealed segment as uncompressed JSON.
  final void Function(String segmentJson, int segment) sink;
  final void Function(ReplayState state)? persist;
  final Random _random;
  ReplayState? _restored;

  ReplayMode mode = ReplayMode.off;
  String? sessionId;
  int segment = 0;

  final List<_Ev> _events = [];
  int _bytes = 0;
  ReplayContext? _segmentContext;
  ReplayContext? _lastContext;
  int _clicks = 0;
  int _errors = 0;
  _Screen? _lastScreen;
  String? _lastFrameKey;
  bool? _lastKeyboard;

  /// Capture the UI at all (recording or buffering for errors).
  bool get isCapturing => mode != ReplayMode.off;

  /// Bytes (uncompressed estimate) currently held.
  int get bufferedBytes => _bytes;
  int get bufferedEvents => _events.length;

  // ───────────────────────── session ─────────────────────────

  /// Re-reads the RUM context; a new session id gets a new sampling decision.
  ReplayMode refreshSession() {
    final ctx = context();
    if (ctx == null || ctx.sessionId.isEmpty) {
      if (sessionId == null) mode = ReplayMode.off;
      return mode;
    }
    _lastContext = ctx;
    if (ctx.sessionId != sessionId) _startSession(ctx);
    return mode;
  }

  void _startSession(ReplayContext ctx) {
    if (mode == ReplayMode.record) _seal();
    _resetBuffer();
    _lastScreen = null;
    _lastFrameKey = null;
    _lastKeyboard = null;
    sessionId = ctx.sessionId;
    final restored = _restored;
    _restored = null;
    if (restored != null && restored.sessionId == ctx.sessionId) {
      // Same session in a new process: continue numbering, keep the decision.
      segment = restored.segment;
      mode = ctx.sampled && options.enabled ? restored.mode : ReplayMode.off;
    } else {
      segment = 0;
      mode = _decide(ctx);
    }
    _persist();
  }

  ReplayMode _decide(ReplayContext ctx) {
    if (!options.enabled || !ctx.sampled) return ReplayMode.off;
    if (_hit(options.sampleRate)) return ReplayMode.record;
    if (_hit(options.onErrorSampleRate)) return ReplayMode.buffer;
    return ReplayMode.off;
  }

  bool _hit(double rate) => rate >= 1 || (rate > 0 && _random.nextDouble() < rate);

  void _persist() {
    final sid = sessionId;
    if (sid == null) return;
    try {
      persist?.call(ReplayState(sid, segment, mode));
    } catch (_) {}
  }

  // ───────────────────────── input ─────────────────────────

  /// A captured frame; [nodesJson] is the full `nodes` array. Recorded only when it differs from the previous one.
  /// [keyboardVisible] null = unknown.
  void onFrame(int ts, int width, int height, double density, String nodesJson, {bool? keyboardVisible}) {
    if (refreshSession() == ReplayMode.off) return;
    final screen = _Screen(width, height, density, _lastContext?.viewName);
    if (screen != _lastScreen) {
      _lastScreen = screen;
      _lastFrameKey = null; // a new screen always gets a frame
      _add(_Ev(ts, WireEvents.screen(ts, width, height, density, screen.name), _screen));
    }
    if (keyboardVisible != null && keyboardVisible != _lastKeyboard) {
      _lastKeyboard = keyboardVisible;
      _add(_Ev(ts, WireEvents.keyboard(ts, keyboardVisible), _keyboard));
    }
    if (nodesJson != _lastFrameKey) {
      _lastFrameKey = nodesJson;
      _add(_Ev(ts, WireEvents.frame(ts, nodesJson), _frame));
    }
    _maybeSeal(ts);
  }

  void onTouch(int ts, double x, double y, String phase) {
    if (mode == ReplayMode.off) return;
    if (phase == 'down') _clicks++;
    _add(_Ev(ts, WireEvents.touch(ts, x, y, phase), _touch));
    _maybeSeal(ts);
  }

  /// RUM ERROR in the current session: buffered sessions flush the last 60 s and keep recording.
  void onError(int ts) {
    switch (mode) {
      case ReplayMode.off:
        return;
      case ReplayMode.buffer:
        _errors++;
        mode = ReplayMode.record;
        _persist();
        _sealChunks();
      case ReplayMode.record:
        _errors++;
    }
  }

  /// Timer (≈ 1 s): seals due segments, trims the ring buffer.
  void tick() {
    final t = now();
    switch (mode) {
      case ReplayMode.record:
        _maybeSeal(t);
      case ReplayMode.buffer:
        _trim(t);
      case ReplayMode.off:
        break;
    }
  }

  /// App went to background: send what is recorded (on-error data stays in memory).
  void flush() {
    if (mode == ReplayMode.record) _seal();
  }

  // ───────────────────────── buffer & segments ─────────────────────────

  void _add(_Ev ev) {
    if (_events.isEmpty) {
      _segmentContext = _lastContext;
      final s = _lastScreen;
      // Each segment starts with the screen so it can be laid out on its own.
      if (ev.type != _screen && s != null && mode == ReplayMode.record) {
        final json = WireEvents.screen(ev.ts, s.width, s.height, s.density, s.name);
        _events.add(_Ev(ev.ts, json, _screen));
        _bytes += json.length;
      }
    }
    _events.add(ev);
    _bytes += ev.json.length;
    if (mode == ReplayMode.buffer) _trim(ev.ts);
  }

  void _maybeSeal(int t) {
    if (mode != ReplayMode.record || _events.isEmpty) return;
    if (t - _events.first.ts >= segmentMaxMs || _bytes >= segmentMaxBytes) _seal();
  }

  /// Ring buffer: the last [bufferMs], plus the latest frame (and screen) older than that so the buffer always
  /// starts with a full wireframe. Hard cap [bufferMaxBytes].
  void _trim(int t) {
    final cutoff = t - bufferMs;
    var lastOldFrame = -1;
    for (var i = 0; i < _events.length; i++) {
      final e = _events[i];
      if (e.ts > cutoff) break;
      if (e.type == _frame) lastOldFrame = i;
    }
    if (lastOldFrame > 0) {
      final head = _events.sublist(0, lastOldFrame);
      final screen = head.lastWhere((e) => e.type == _screen, orElse: () => _Ev(0, '', 0));
      for (final e in head) {
        _bytes -= e.json.length;
      }
      _events.removeRange(0, lastOldFrame);
      if (screen.type == _screen) {
        // Re-stamp the carried-over screen to the first kept event so start_ts reflects the buffered window.
        final ts = _events.first.ts;
        final restamped = _Ev(ts, screen.json.replaceFirst('"ts":${screen.ts}', '"ts":$ts'), _screen);
        _events.insert(0, restamped);
        _bytes += restamped.json.length;
      }
    }
    while (_bytes > bufferMaxBytes && _events.length > 1) {
      _bytes -= _events.removeAt(0).json.length;
    }
  }

  /// Flushes the whole buffer as segments of ≤ 256 KB.
  void _sealChunks() {
    while (_events.isNotEmpty) {
      if (_bytes <= segmentMaxBytes) {
        _seal();
        return;
      }
      var n = 0;
      var size = 0;
      while (n < _events.length && (n == 0 || size + _events[n].json.length <= segmentMaxBytes)) {
        size += _events[n].json.length;
        n++;
      }
      final chunk = _events.sublist(0, n);
      _events.removeRange(0, n);
      _bytes -= size;
      _write(chunk, _clicks, _errors);
      _clicks = 0;
      _errors = 0;
      _segmentContext = _lastContext;
    }
  }

  void _seal() {
    if (_events.isEmpty) return;
    final chunk = List.of(_events);
    final c = _clicks;
    final e = _errors;
    _resetBuffer();
    _write(chunk, c, e);
  }

  void _resetBuffer() {
    _events.clear();
    _bytes = 0;
    _clicks = 0;
    _errors = 0;
  }

  void _write(List<_Ev> chunk, int clicks, int errors) {
    final sid = sessionId;
    if (sid == null) return;
    try {
      final json = _segmentJson(sid, segment, resource(), _segmentContext ?? _lastContext, chunk, clicks, errors);
      sink(json, segment);
    } catch (_) {
      // never reach the host app
    }
    segment++;
    _persist();
  }

  static String _segmentJson(
    String sessionId,
    int segment,
    ReplayResource res,
    ReplayContext? ctx,
    List<_Ev> events,
    int clicks,
    int errors,
  ) {
    final header = <String, Object?>{
      'resource': res.toJson(),
      'session_id': sessionId,
      'segment': segment,
      'start_ts': events.first.ts,
      'end_ts': events.map((e) => e.ts).reduce(max),
      'format': 'wireframe',
      'has_full_snapshot': events.any((e) => e.type == _frame),
      if (ctx?.viewId != null) 'view_id': ctx!.viewId,
      if (ctx?.viewName != null) 'view_name': ctx!.viewName,
      if (ctx?.userId != null) 'user_id': ctx!.userId,
      if (ctx != null && ctx.anonymousId.isNotEmpty) 'anonymous_id': ctx.anonymousId,
      // Rage clicks / inputs are not detected on mobile wireframes (optional fields, omitted).
      'stats': {'clicks': clicks, 'errors': errors},
    };
    final head = jsonEncode(header);
    final sb = StringBuffer(head.substring(0, head.length - 1))..write(',"events":[');
    for (var i = 0; i < events.length; i++) {
      if (i > 0) sb.write(',');
      sb.write(events[i].json);
    }
    sb.write(']}');
    return sb.toString();
  }
}
