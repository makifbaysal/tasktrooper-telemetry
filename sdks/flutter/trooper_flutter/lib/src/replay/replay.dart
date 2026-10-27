import 'dart:async';
import 'dart:convert';
import 'dart:math';
import 'dart:ui' as ui;

import 'package:clock/clock.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/gestures.dart';
import 'package:flutter/scheduler.dart';
import 'package:flutter/services.dart';
import 'package:flutter/widgets.dart';

import '../client.dart';
import '../native_bridge.dart';
import '../platform/platform.dart';
import '../storage.dart';
import '../transport.dart';
import '../version.dart';
import 'engine.dart';
import 'mapper.dart';
import 'options.dart';
import 'uploader.dart';
import 'wireframe.dart';

/// Capture cost counters (main isolate time per capture, mapping + serialization).
class ReplayStats {
  int captures = 0;
  int truncated = 0;
  int lastCostUs = 0;
  int maxCostUs = 0;
  int totalCostUs = 0;
  int lastNodes = 0;
  int lastElements = 0;

  int get avgCostUs => captures == 0 ? 0 : totalCostUs ~/ captures;

  @override
  String toString() =>
      'captures=$captures avg=${avgCostUs}us max=${maxCostUs}us last=${lastCostUs}us '
      'nodes=$lastNodes elements=$lastElements truncated=$truncated';
}

/// Session replay for Flutter (wireframe, contracts.md §23). Started by
/// `TaskTrooper.init(config, replay: TrooperReplayOptions(...))`; these members are for inspection and control.
abstract final class TrooperReplay {
  static ReplayRecorder? _recorder;
  static Future<void>? _installing;

  /// The running recorder (null when replay is off, not installed or not supported on this platform).
  static ReplayRecorder? get recorder => _recorder;

  /// True while the current session is recorded (not just buffered for errors).
  static bool get isRecording => _recorder?.engine.mode == ReplayMode.record;

  static ReplayStats? get stats => _recorder?.stats;

  /// Installs replay for [client]. No-op on web/desktop, when disabled, when already installed, or when the
  /// native SDK's own replay is running in this app (it would record the FlutterView as one box and both would
  /// write the same session's segments). Never throws.
  static Future<void> install(
    TrooperClient client,
    TrooperReplayOptions options, {
    TrooperStorage? storage,
    ReplaySegmentStore? segmentStore,
    RumTransport? transport,
    MethodChannel? channel,
    bool checkNativeReplay = true,
    bool debug = false,
  }) {
    if (_recorder != null || _installing != null || !options.enabled) return Future.value();
    final f = _install(client, options, storage, segmentStore, transport, channel, checkNativeReplay, debug);
    _installing = f;
    return f.whenComplete(() => _installing = null);
  }

  static Future<void> _install(
    TrooperClient client,
    TrooperReplayOptions options,
    TrooperStorage? storage,
    ReplaySegmentStore? segmentStore,
    RumTransport? transport,
    MethodChannel? channel,
    bool checkNativeReplay,
    bool debug,
  ) async {
    try {
      final platform = replayPlatform();
      if (platform == null) {
        if (debug) debugPrint('[trooper] replay: not supported on this platform (web uses @trooper/replay)');
        return;
      }
      if (checkNativeReplay && await TrooperNativeBridge(client, channel: channel).isNativeReplayActive()) {
        debugPrint(
          '[trooper] replay: the native TaskTrooper replay is installed in this app; Flutter replay is not started. '
          'Remove TrooperReplay.install / TaskTrooper/Replay from the host app (it cannot see Flutter UI).',
        );
        return;
      }
      final kv = storage ?? MemoryStorage();
      final restored = ReplayState.decode(await kv.readValue(ReplayRecorder.stateKey));
      final segments = segmentStore ?? await createReplaySegmentStore(options.maxQueueBytes);
      final uploader = ReplayUploader(
        store: segments,
        transport: transport ?? RumTransport(dsn: client.dsn),
        url: client.dsn.replayEndpoint,
        gzipped: gzipBytes(const [0]) != null,
      );
      final recorder = ReplayRecorder(
        client: client,
        options: options,
        uploader: uploader,
        storage: kv,
        restored: restored,
        platform: platform,
        debug: debug,
      );
      _recorder = recorder;
      recorder.start();
      unawaited(uploader.drain()); // segments left by previous runs
    } catch (e) {
      if (debug) debugPrint('[trooper] replay: install failed: $e');
    }
  }

  /// Seals the current segment and uploads pending segments.
  static Future<void> flush() async {
    final r = _recorder;
    if (r == null) return;
    await r.flush();
  }

  /// Stops recording (pending segments stay on disk and are sent on the next start).
  static Future<void> close() async {
    final r = _recorder;
    _recorder = null;
    r?.stop();
  }

  /// `android` | `ios`, or null when Flutter replay does not run on this platform.
  static String? replayPlatform() {
    if (kIsWeb) return null;
    return switch (defaultTargetPlatform) {
      TargetPlatform.android => 'android',
      TargetPlatform.iOS => 'ios',
      _ => null,
    };
  }
}

/// Binds the [ReplayEngine] to Flutter: dirty flag from a persistent frame callback, captures at most every
/// [TrooperReplayOptions.minCaptureInterval] (raised when captures are expensive, ≈ 1 % main-isolate budget),
/// touches from the global pointer route, keyboard from view insets, lifecycle flushes.
class ReplayRecorder with WidgetsBindingObserver {
  ReplayRecorder({
    required this.client,
    required this.options,
    required this.uploader,
    required this.storage,
    required String platform,
    ReplayState? restored,
    this.debug = false,
    Random? random,
  }) : mapper = ReplayMapper(
         maskAllText: options.maskAllText,
         maxNodes: options.maxNodes,
         // JIT (debug) code is several times slower than AOT: keep debug frames complete.
         budget: kDebugMode ? options.captureBudget * 4 : options.captureBudget,
       ) {
    engine = ReplayEngine(
      options: options,
      now: () => clock.now().millisecondsSinceEpoch,
      context: _context,
      resource: () => ReplayResource(
        service: client.config.app,
        env: client.config.environment,
        version: client.config.version,
        platform: platform,
        sdkVersion: sdkVersion,
      ),
      sink: _onSegment,
      persist: _persist,
      restored: restored,
      random: random,
    );
    _interval = options.minCaptureInterval;
  }

  static const stateKey = 'replay_state';
  static const moveInterval = Duration(milliseconds: 100);
  static const maxInterval = Duration(seconds: 5);
  static const sessionCheckInterval = Duration(seconds: 5);

  final TrooperClient client;
  final TrooperReplayOptions options;
  final ReplayUploader uploader;
  final TrooperStorage storage;
  final ReplayMapper mapper;
  final bool debug;
  late final ReplayEngine engine;
  final ReplayStats stats = ReplayStats();

  bool _running = false;
  bool _dirty = true;
  Timer? _captureTimer;
  Timer? _tickTimer;
  DateTime _lastCapture = DateTime.fromMillisecondsSinceEpoch(0);
  DateTime _lastSessionCheck = DateTime.fromMillisecondsSinceEpoch(0);
  Duration _lastMove = Duration.zero;
  late Duration _interval;
  Future<void> _persisting = Future.value();
  final Set<Future<void>> _pending = {};

  /// Current throttle between captures.
  Duration get captureInterval => _interval;

  ReplayContext? _context() {
    final id = client.session.id;
    if (id == null) return null;
    return ReplayContext(
      sessionId: id,
      sampled: client.session.sampled,
      anonymousId: client.anonymousId,
      viewId: client.currentViewId,
      viewName: client.currentViewName,
      userId: client.user?.id,
    );
  }

  void start() {
    if (_running) return;
    _running = true;
    try {
      SchedulerBinding.instance.addPersistentFrameCallback(_onFrame);
      GestureBinding.instance.pointerRouter.addGlobalRoute(_onPointer);
      WidgetsBinding.instance.addObserver(this);
      client.errorListeners.add(_onError);
      _tickTimer = Timer.periodic(const Duration(seconds: 1), (_) => _tick());
      engine.refreshSession();
      markDirty();
    } catch (_) {}
  }

  void stop() {
    if (!_running) return;
    _running = false;
    try {
      // Persistent frame callbacks cannot be removed; _onFrame checks _running.
      GestureBinding.instance.pointerRouter.removeGlobalRoute(_onPointer);
      WidgetsBinding.instance.removeObserver(this);
      client.errorListeners.remove(_onError);
      _captureTimer?.cancel();
      _tickTimer?.cancel();
      engine.flush();
      uploader.close();
    } catch (_) {}
  }

  Future<void> flush() async {
    try {
      engine.flush();
      await Future.wait(List.of(_pending));
      await uploader.drain();
    } catch (_) {}
  }

  void _onFrame(Duration _) {
    if (_running) markDirty();
  }

  /// Something may have changed on screen; capture at the next allowed time.
  void markDirty() {
    _dirty = true;
    if (!_running || _captureTimer != null) return;
    final wait = _lastCapture.add(_interval).difference(clock.now());
    _captureTimer = Timer(wait.isNegative ? Duration.zero : wait, _capture);
  }

  void _capture() {
    _captureTimer = null;
    if (!_running || !_dirty) return;
    try {
      if (!engine.isCapturing) {
        // Not recording this session; a new session gets a new decision (checked in _tick).
        return;
      }
      if (SchedulerBinding.instance.schedulerPhase != SchedulerPhase.idle) {
        markDirty();
        return;
      }
      captureNow();
    } catch (_) {}
  }

  /// Captures one frame now (also used by tests). Returns the capture, or null when nothing was captured.
  ReplayCapture? captureNow() {
    try {
      final root = WidgetsBinding.instance.rootElement;
      final view =
          ui.PlatformDispatcher.instance.implicitView ??
          (ui.PlatformDispatcher.instance.views.isEmpty ? null : ui.PlatformDispatcher.instance.views.first);
      if (root == null || view == null) return null;
      _dirty = false;
      final dpr = view.devicePixelRatio <= 0 ? 1.0 : view.devicePixelRatio;
      final size = view.physicalSize / dpr;
      final watch = Stopwatch()..start();
      final capture = mapper.capture(root, size);
      if (capture.nodes.isEmpty) {
        // Nothing laid out yet (before the first frame): wait for a real one.
        _dirty = true;
        return null;
      }
      final nodesJson = WireEvents.nodes(capture.nodes);
      watch.stop();
      _lastCapture = clock.now();
      final costUs = watch.elapsedMicroseconds;
      stats
        ..captures += 1
        ..lastCostUs = costUs
        ..maxCostUs = max(stats.maxCostUs, costUs)
        ..totalCostUs += costUs
        ..lastNodes = capture.nodes.length
        ..lastElements = capture.elements;
      if (capture.truncated) stats.truncated++;
      // Budget: ≈ 1 % of main-isolate time (cost × 100), never faster than minCaptureInterval, never slower than 5 s.
      var next = Duration(microseconds: costUs * 100);
      if (next < options.minCaptureInterval) next = options.minCaptureInterval;
      if (next > maxInterval) next = maxInterval;
      if (capture.truncated && next < const Duration(seconds: 2)) next = const Duration(seconds: 2);
      _interval = next;
      if (debug && (stats.captures == 1 || stats.captures % 20 == 0)) debugPrint('[trooper] replay: $stats');
      final keyboard = view.viewInsets.bottom > 0;
      engine.onFrame(
        _lastCapture.millisecondsSinceEpoch,
        size.width.round(),
        size.height.round(),
        dpr,
        nodesJson,
        keyboardVisible: keyboard,
      );
      return capture;
    } catch (_) {
      return null;
    }
  }

  void _onPointer(PointerEvent e) {
    try {
      if (!_running || !engine.isCapturing) return;
      final String phase;
      if (e is PointerDownEvent) {
        phase = 'down';
      } else if (e is PointerMoveEvent) {
        if (e.timeStamp - _lastMove < moveInterval) return;
        _lastMove = e.timeStamp;
        phase = 'move';
      } else if (e is PointerUpEvent || e is PointerCancelEvent) {
        phase = 'up';
      } else {
        return;
      }
      engine.onTouch(clock.now().millisecondsSinceEpoch, e.position.dx, e.position.dy, phase);
    } catch (_) {}
  }

  void _onError(int ts) {
    try {
      engine.onError(ts);
    } catch (_) {}
  }

  void _tick() {
    try {
      final wasCapturing = engine.isCapturing;
      final now = clock.now();
      if (!wasCapturing && now.difference(_lastSessionCheck) >= sessionCheckInterval) {
        _lastSessionCheck = now;
        engine.refreshSession();
      } else if (wasCapturing) {
        engine.refreshSession(); // session rotation seals the old session's segment
      }
      engine.tick();
      if (!wasCapturing && engine.isCapturing) markDirty();
    } catch (_) {}
  }

  @override
  void didChangeMetrics() {
    markDirty(); // rotation, keyboard (view insets)
  }

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    switch (state) {
      case AppLifecycleState.paused:
      case AppLifecycleState.hidden:
      case AppLifecycleState.detached:
        unawaited(flush());
      case AppLifecycleState.resumed:
        markDirty();
      case AppLifecycleState.inactive:
        break;
    }
  }

  void _onSegment(String json, int segment) {
    final f = _enqueue(json);
    _pending.add(f);
    unawaited(f.whenComplete(() => _pending.remove(f)));
  }

  Future<void> _enqueue(String json) async {
    try {
      final raw = utf8.encode(json);
      final gz = gzipBytes(raw);
      await uploader.enqueue(gz ?? raw);
    } catch (_) {}
  }

  void _persist(ReplayState state) {
    final encoded = state.encode();
    _persisting = _persisting.then((_) => storage.writeValue(stateKey, encoded)).catchError((Object _) {});
  }
}
