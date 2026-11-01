import 'dart:async';
import 'dart:math';

import 'package:clock/clock.dart';
import 'package:flutter/foundation.dart';

import 'config.dart';
import 'dsn.dart';
import 'frame_stats.dart';
import 'ids.dart';
import 'model.dart';
import 'pipeline.dart';
import 'session.dart';
import 'trace.dart';

/// Session/view/user context shared with native SDKs.
class TrooperContext {
  const TrooperContext({
    required this.sessionId,
    required this.anonymousId,
    this.viewId,
    this.viewName,
    this.userId,
    this.sampled = true,
  });
  final String sessionId;
  final String anonymousId;
  final String? viewId;
  final String? viewName;
  final String? userId;

  /// RUM sampling decision of the session (contracts.md §9); the native SDK adopts it with the session.
  final bool sampled;

  Map<String, Object?> toMap() => {
    'session_id': sessionId,
    'anonymous_id': anonymousId,
    'view_id': viewId,
    'view_name': viewName,
    'user_id': userId,
    'sampled': sampled,
  };
}

class _View {
  _View(this.id, this.name, this.startedAt, this.attributes);
  final String id;
  final String name;
  final DateTime startedAt;
  final Map<String, String> attributes;
  final FrameStats frames = FrameStats();
  bool startEmitted = false;
  bool isInitial = false;
}

/// The RUM engine: builds events with session/view/user context and feeds
/// the [RumPipeline]. Every public method swallows its own errors.
///
/// Usually accessed through `TaskTrooper`; constructed directly in tests.
class TrooperClient {
  TrooperClient({
    required this.config,
    required this.dsn,
    required this.pipeline,
    required this.anonymousId,
    Random? random,
  }) : _random = random ?? Random(),
       session = SessionManager(
         inactivityTimeout: config.sessionInactivityTimeout,
         maxDuration: config.sessionMaxDuration,
         sampleRate: config.sampleRate,
         random: random,
       );

  final TrooperConfig config;
  final Dsn dsn;
  final RumPipeline pipeline;
  final String anonymousId;
  final SessionManager session;
  final Random _random;

  RumUser? _user;
  _View? _view;
  bool _hadViewInSession = false;
  String? _backgroundedViewName;
  Map<String, String> _backgroundedViewAttrs = const {};
  bool _inBackground = false;

  /// Count of internal SDK errors swallowed (internal metric, AGENTS.md rule 5).
  int internalErrors = 0;

  /// Called after each recorded ERROR with its timestamp (unix ms); used by session replay's on-error buffer.
  final List<void Function(int tsMs)> errorListeners = [];

  /// Called whenever session, view or user changes (native bridge hook).
  void Function(TrooperContext context)? onContextChanged;

  String? get currentViewId => _view?.id;
  String? get currentViewName => _view?.name;
  RumUser? get user => _user;

  TrooperContext get context => TrooperContext(
    sessionId: session.id ?? '',
    anonymousId: anonymousId,
    viewId: _view?.id,
    viewName: _view?.name,
    userId: _user?.id,
    sampled: session.sampled,
  );

  void _internalError(Object e, [StackTrace? s]) {
    internalErrors++;
    if (config.debug) debugPrint('[trooper] internal error: $e');
  }

  void _guard(void Function() body) {
    try {
      body();
    } catch (e, s) {
      _internalError(e, s);
    }
  }

  void _notifyError() {
    final ts = clock.now().millisecondsSinceEpoch;
    for (final l in List.of(errorListeners)) {
      try {
        l(ts);
      } catch (e) {
        _internalError(e);
      }
    }
  }

  void _notifyContext() {
    final cb = onContextChanged;
    if (cb == null) return;
    try {
      cb(context);
    } catch (e) {
      _internalError(e);
    }
  }

  // ───────────────────────── event plumbing ─────────────────────────

  /// Ensures a live session; on rotation emits SESSION_START and restarts the
  /// current view inside the new session.
  void _ensureSession() {
    if (!session.isExpired) {
      session.touch();
      return;
    }
    final hadSession = session.id != null;
    final view = _view;
    if (hadSession && view != null) _endView(view);
    session.rotate();
    _hadViewInSession = false;
    _write(RumEventType.sessionStart, null);
    if (hadSession && view != null) {
      _beginView(view.name, view.attributes, awaitFirstFrame: false);
    }
    _notifyContext();
  }

  void _emit(RumEventType type, RumPayload? payload, {DateTime? ts, Map<String, String>? attributes}) {
    _ensureSession();
    _write(type, payload, ts: ts, attributes: attributes);
  }

  void _write(RumEventType type, RumPayload? payload, {DateTime? ts, Map<String, String>? attributes, _View? view}) {
    if (!session.sampled) return;
    final v = view ?? _view;
    RumEvent? event = RumEvent(
      type: type,
      tsMs: (ts ?? clock.now()).millisecondsSinceEpoch,
      sessionId: session.id ?? '',
      viewId: v?.id ?? '',
      viewName: v?.name ?? '',
      anonymousId: anonymousId,
      user: _user,
      payload: payload,
      attributes: attributes == null ? null : Map.of(attributes),
    );
    final before = config.beforeSend;
    if (before != null) {
      try {
        event = before(event);
      } catch (e) {
        _internalError(e);
        return;
      }
    }
    if (event == null) return;
    pipeline.add(event.toJson());
  }

  // ───────────────────────── views ─────────────────────────

  /// Starts a view (screen). Ends the current one first. With
  /// [awaitFirstFrame], VIEW_START is held back until [markViewLoaded] so it
  /// can carry `loading_time_ms`. Returns the view id ('' on failure).
  String startView(String name, {Map<String, String>? attributes, bool awaitFirstFrame = false}) {
    try {
      _inBackground = false;
      final current = _view;
      if (current != null) _endView(current); // still in the session it belongs to
      _ensureSession();
      final v = _beginView(name, attributes ?? const {}, awaitFirstFrame: awaitFirstFrame);
      _notifyContext();
      return v.id;
    } catch (e, s) {
      _internalError(e, s);
      return '';
    }
  }

  _View _beginView(String name, Map<String, String> attributes, {required bool awaitFirstFrame}) {
    final v = _View(uuidV4(_random), name, clock.now(), Map.of(attributes));
    v.isInitial = !_hadViewInSession;
    _hadViewInSession = true;
    _view = v;
    if (!awaitFirstFrame) _emitViewStart(v, 0);
    return v;
  }

  void _emitViewStart(_View v, double loadingMs) {
    v.startEmitted = true;
    _write(
      RumEventType.viewStart,
      ViewPayload(loadingTimeMs: loadingMs, isInitial: v.isInitial),
      ts: v.startedAt,
      attributes: v.attributes,
      view: v,
    );
  }

  /// Marks the first frame of [viewId] as rendered; emits its VIEW_START with
  /// the elapsed loading time.
  void markViewLoaded(String viewId) {
    _guard(() {
      final v = _view;
      if (v == null || v.id != viewId || v.startEmitted) return;
      _emitViewStart(v, clock.now().difference(v.startedAt).inMicroseconds / 1000);
    });
  }

  /// Ends the current view (if [name] is given, only when it matches).
  void stopView([String? name]) {
    _guard(() {
      final v = _view;
      if (v == null || (name != null && v.name != name)) return;
      _endView(v);
      _view = null;
      _notifyContext();
    });
  }

  void _endView(_View v) {
    if (!v.startEmitted) _emitViewStart(v, 0);
    final f = v.frames;
    if (f.total > 0) {
      final attrs = {'total_frames': '${f.total}'};
      _write(
        RumEventType.vital,
        VitalPayload(name: 'SLOW_FRAMES', value: f.slow.toDouble(), rating: f.slowRating),
        attributes: attrs,
        view: v,
      );
      _write(
        RumEventType.vital,
        VitalPayload(name: 'FROZEN_FRAMES', value: f.frozen.toDouble(), rating: f.frozenRating),
        attributes: attrs,
        view: v,
      );
    }
    final spent = clock.now().difference(v.startedAt).inMicroseconds / 1000;
    _write(
      RumEventType.viewEnd,
      ViewPayload(timeSpentMs: spent),
      attributes: v.attributes,
      view: v,
    );
    if (identical(_view, v)) _view = null;
  }

  // ───────────────────────── lifecycle ─────────────────────────

  /// App went to background: end the view and flush.
  Future<void> onBackground() async {
    try {
      if (_inBackground) return;
      _inBackground = true;
      final v = _view;
      if (v != null) {
        _backgroundedViewName = v.name;
        _backgroundedViewAttrs = v.attributes;
        _endView(v);
      }
      await pipeline.flush();
    } catch (e, s) {
      _internalError(e, s);
    }
  }

  /// App returned to foreground: (maybe) rotate the session, restart the view.
  void onForeground() {
    _guard(() {
      if (!_inBackground) return;
      _inBackground = false;
      final name = _backgroundedViewName;
      _backgroundedViewName = null;
      if (name != null && _view == null) startView(name, attributes: _backgroundedViewAttrs);
    });
  }

  // ───────────────────────── user / custom ─────────────────────────

  /// Sets (or clears with null) the user attached to subsequent events.
  void identify(String? id, {Map<String, String>? traits}) {
    _guard(() {
      _user = (id == null || id.isEmpty) ? null : RumUser(id: id, traits: traits);
      _notifyContext();
    });
  }

  void track(String name, {Map<String, String>? props, double value = 0, Map<String, String>? attributes}) {
    _guard(
      () => _emit(
        RumEventType.custom,
        CustomPayload(name: name, props: props, value: value),
        attributes: attributes,
      ),
    );
  }

  void addAction(String name, {String target = '', Map<String, String>? attributes}) {
    _guard(
      () => _emit(
        RumEventType.action,
        ActionPayload(name: name, target: target),
        attributes: attributes,
      ),
    );
  }

  void addVital(String name, double value, {String rating = '', Map<String, String>? attributes}) {
    _guard(
      () => _emit(
        RumEventType.vital,
        VitalPayload(name: name, value: value, rating: rating),
        attributes: attributes,
      ),
    );
  }

  // ───────────────────────── errors ─────────────────────────

  /// Records an error. Dart errors are never crashes (`is_crash` false): the
  /// app keeps running. Native crashes come from the native SDKs.
  void captureError(
    Object error,
    StackTrace? stack, {
    String source = 'source',
    bool isCrash = false,
    String? context,
    Map<String, String>? attributes,
  }) {
    _guard(() {
      final attrs = {...?attributes, if (context != null && context.isNotEmpty) 'context': context};
      _emit(
        RumEventType.error,
        ErrorPayload(
          type: error.runtimeType.toString(),
          message: _truncate(error.toString(), 4096),
          stack: _truncate(stack?.toString() ?? '', 64 * 1024),
          source: source,
          isCrash: isCrash,
          thread: 'main',
        ),
        attributes: attrs,
      );
      if (session.sampled) _notifyError();
    });
  }

  static String _truncate(String s, int max) => s.length <= max ? s : s.substring(0, max);

  // ───────────────────────── network ─────────────────────────

  /// Whether a `traceparent` header should be added for [url].
  bool shouldPropagate(Uri url) {
    try {
      return !isIngestRequest(url) && matchesPropagationTarget(url, config.tracePropagationTargets);
    } catch (e) {
      _internalError(e);
      return false;
    }
  }

  /// Requests to our own ingest endpoint are never instrumented.
  bool isIngestRequest(Uri url) =>
      url.host == dsn.endpoint.host &&
      url.port == dsn.endpoint.port &&
      (url.path.endsWith('/v1/rum') || url.path.endsWith('/v1/replay'));

  /// Records a RESOURCE (network request) event.
  void recordResource({
    required Uri url,
    required String method,
    required int statusCode,
    required Duration duration,
    required DateTime startedAt,
    int requestBytes = 0,
    int responseBytes = 0,
    TraceContext? trace,
    String initiator = 'http',
  }) {
    _guard(() {
      if (isIngestRequest(url)) return;
      _emit(
        RumEventType.resource,
        ResourcePayload(
          url: sanitizeUrl(url),
          method: method.toUpperCase(),
          statusCode: statusCode,
          durationMs: duration.inMicroseconds / 1000,
          requestBytes: max(0, requestBytes),
          responseBytes: max(0, responseBytes),
          traceId: trace?.traceId ?? '',
          spanId: trace?.spanId ?? '',
          initiator: initiator,
        ),
        ts: startedAt,
      );
    });
  }

  // ───────────────────────── vitals ─────────────────────────

  /// Adds frame timings to the current view's slow/frozen counters.
  void recordFrame({required double buildMs, required double rasterMs, required double totalMs}) {
    _guard(() => _view?.frames.add(buildMs: buildMs, rasterMs: rasterMs, totalMs: totalMs));
  }

  void recordAppStart(Duration duration, {String kind = 'cold', Map<String, String>? attributes}) {
    _guard(
      () => _emit(
        RumEventType.appStart,
        AppStartPayload(kind: kind, durationMs: duration.inMicroseconds / 1000),
        attributes: attributes,
      ),
    );
  }

  Future<void> flush() async {
    try {
      await pipeline.flush();
    } catch (e, s) {
      _internalError(e, s);
    }
  }

  Future<void> close() async {
    try {
      await pipeline.close();
    } catch (e, s) {
      _internalError(e, s);
    }
  }
}
