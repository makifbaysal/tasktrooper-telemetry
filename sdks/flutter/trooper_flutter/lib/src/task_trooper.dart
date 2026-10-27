import 'dart:async';

import 'package:clock/clock.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/scheduler.dart';
import 'package:flutter/services.dart';
import 'package:flutter/widgets.dart';
import 'package:http/http.dart' as http;

import 'client.dart';
import 'config.dart';
import 'device.dart';
import 'dsn.dart';
import 'errors.dart';
import 'ids.dart';
import 'native_bridge.dart';
import 'pipeline.dart';
import 'platform/platform.dart';
import 'replay/options.dart';
import 'replay/replay.dart';
import 'storage.dart';
import 'transport.dart';

/// Entry point of the TaskTrooper Flutter SDK.
///
/// ```dart
/// void main() => TaskTrooper.init(
///       TrooperConfig(dsn: '...', app: 'courier-app', version: '1.4.2'),
///       appRunner: () => runApp(const MyApp()),
///     );
/// ```
///
/// All methods are safe to call before/without [init] (they no-op) and never
/// throw.
abstract final class TaskTrooper {
  static TrooperClient? _client;
  static VoidCallback? _uninstallErrors;
  static AppLifecycleListener? _lifecycle;
  static TimingsCallback? _timings;

  /// The active client, or null if the SDK is not (successfully) initialized.
  static TrooperClient? get client => _client;
  static bool get isInitialized => _client != null;

  /// Initializes the SDK. With [appRunner], the app runs inside a guarded
  /// zone (`runZonedGuarded`) so uncaught async errors are reported too;
  /// `WidgetsFlutterBinding.ensureInitialized()` is called inside that zone.
  ///
  /// The returned future completes once the SDK is set up and [appRunner]
  /// has been called. An invalid config disables the SDK but still runs the
  /// app.
  static Future<void> init(TrooperConfig config, {FutureOr<void> Function()? appRunner, TrooperReplayOptions? replay}) {
    final started = clock.now();
    final done = Completer<void>();
    if (appRunner == null) {
      return _setup(config, started, replay);
    }
    runZonedGuarded(
      () async {
        try {
          WidgetsFlutterBinding.ensureInitialized();
          await _setup(config, started, replay);
        } catch (_) {}
        try {
          await appRunner();
        } finally {
          _afterRunApp(config, started);
          if (!done.isCompleted) done.complete();
        }
      },
      (error, stack) {
        if (!done.isCompleted) done.complete();
        _client?.captureError(error, stack, context: 'zone');
        if (config.debug || _client == null) {
          // Keep the default visibility of uncaught errors.
          FlutterError.dumpErrorToConsole(FlutterErrorDetails(exception: error, stack: stack, library: 'zone'));
        }
      },
    );
    return done.future;
  }

  static Future<void> _setup(TrooperConfig config, DateTime started, TrooperReplayOptions? replay) async {
    if (_client != null) return;
    try {
      WidgetsFlutterBinding.ensureInitialized();
      final dsn = Dsn.parse(config.dsn);
      final storage = await createPlatformStorage();
      var anonymousId = await storage.readValue('anonymous_id');
      if (anonymousId == null || anonymousId.isEmpty) {
        anonymousId = uuidV4();
        await storage.writeValue('anonymous_id', anonymousId);
      }
      final resource = await collectResource(config);
      late final TrooperClient client;
      final pipeline = RumPipeline(
        transport: RumTransport(dsn: dsn, gzip: !kIsWeb),
        storage: storage,
        resource: resource,
        maxBatchSize: config.maxBatchSize,
        flushInterval: config.flushInterval,
        maxQueueBytes: config.maxQueueBytes,
        onInternalError: (e) => client.internalErrors++,
      );
      client = TrooperClient(config: config, dsn: dsn, pipeline: pipeline, anonymousId: anonymousId);
      _client = client;
      unawaited(pipeline.restore());
      _installHooks(config, client);
      if (replay != null && replay.enabled) {
        unawaited(
          TrooperReplay.install(
            client,
            replay,
            storage: storage,
            checkNativeReplay: config.nativeBridge,
            debug: config.debug,
          ),
        );
      }
    } catch (e) {
      _client = null;
      if (config.debug) debugPrint('[trooper] init failed, SDK disabled: $e');
    }
  }

  static void _installHooks(TrooperConfig config, TrooperClient client) {
    if (config.nativeBridge) {
      try {
        TrooperNativeBridge(client).attach();
      } catch (_) {}
    }
    if (config.captureErrors) {
      try {
        _uninstallErrors = installErrorHandlers(
          (error, stack, {context}) => client.captureError(error, stack, context: context),
        );
      } catch (_) {}
    }
    if (config.trackLifecycle) {
      try {
        _lifecycle = AppLifecycleListener(
          onStateChange: (state) {
            switch (state) {
              case AppLifecycleState.hidden:
              case AppLifecycleState.paused:
              case AppLifecycleState.detached:
                unawaited(client.onBackground());
              case AppLifecycleState.resumed:
                client.onForeground();
              case AppLifecycleState.inactive:
                break;
            }
          },
        );
      } catch (_) {}
    }
    if (config.trackFrames) {
      try {
        void onTimings(List<FrameTiming> timings) {
          for (final t in timings) {
            client.recordFrame(
              buildMs: t.buildDuration.inMicroseconds / 1000,
              rasterMs: t.rasterDuration.inMicroseconds / 1000,
              totalMs: t.totalSpan.inMicroseconds / 1000,
            );
          }
        }

        _timings = onTimings;
        SchedulerBinding.instance.addTimingsCallback(onTimings);
      } catch (_) {}
    }
  }

  /// APP_START: from process start (native bridge, when available) or from
  /// [init] (≈ Dart `main`) to the first rasterized frame.
  static void _afterRunApp(TrooperConfig config, DateTime initStarted) {
    final client = _client;
    if (client == null || !config.trackAppStart) return;
    try {
      unawaited(
        recordColdStart(
          client,
          initStarted,
          nativeBridge: config.nativeBridge,
          firstFrame: WidgetsBinding.instance.waitUntilFirstFrameRasterized,
        ),
      );
    } catch (_) {}
  }

  /// Records the cold APP_START once [firstFrame] completes; the start is the
  /// native process start when the bridge answers `getProcessStartTime`.
  @visibleForTesting
  static Future<void> recordColdStart(
    TrooperClient client,
    DateTime initStarted, {
    bool nativeBridge = true,
    Future<void>? firstFrame,
    MethodChannel? channel,
  }) async {
    try {
      final nativeStart = nativeBridge ? await TrooperNativeBridge(client, channel: channel).processStartTime() : null;
      if (firstFrame != null) await firstFrame;
      final start = nativeStart ?? initStarted;
      client.recordAppStart(
        clock.now().difference(start),
        kind: 'cold',
        attributes: {'start_reference': nativeStart != null ? 'process' : 'dart_init'},
      );
    } catch (_) {}
  }

  /// Attach a user to subsequent events (pass null to clear). Traits are
  /// opt-in; do not put PII there unless you mean to.
  static void identify(String? id, {Map<String, String>? traits}) => _client?.identify(id, traits: traits);

  /// Custom event (`CUSTOM`).
  static void track(String name, {Map<String, String>? props, double value = 0}) =>
      _client?.track(name, props: props, value: value);

  /// Report a handled error.
  static void captureError(Object error, [StackTrace? stack, Map<String, String>? attributes]) =>
      _client?.captureError(error, stack ?? StackTrace.current, attributes: attributes);

  /// Manual view tracking (when not using [TrooperNavigatorObserver]).
  static String? startView(String name, {Map<String, String>? attributes}) =>
      _client?.startView(name, attributes: attributes);

  static void stopView([String? name]) => _client?.stopView(name);

  /// User action (tap) — `ACTION`.
  static void addAction(String name, {String target = ''}) => _client?.addAction(name, target: target);

  /// Upload everything buffered now.
  static Future<void> flush() => _client?.flush() ?? Future.value();

  /// Disables the SDK, removes hooks, persists pending events.
  static Future<void> close() async {
    final c = _client;
    _client = null;
    try {
      await TrooperReplay.close();
    } catch (_) {}
    try {
      _uninstallErrors?.call();
      _uninstallErrors = null;
      _lifecycle?.dispose();
      _lifecycle = null;
      final t = _timings;
      if (t != null) SchedulerBinding.instance.removeTimingsCallback(t);
      _timings = null;
      await c?.close();
    } catch (_) {}
  }

  /// Installs a pre-built client (tests, custom setups).
  @visibleForTesting
  static void debugSetClient(TrooperClient? client) => _client = client;

  /// Builds a client without Flutter bindings or platform storage.
  @visibleForTesting
  static TrooperClient createClient(
    TrooperConfig config, {
    http.Client? httpClient,
    TrooperStorage? storage,
    String anonymousId = 'test-anon',
  }) {
    final dsn = Dsn.parse(config.dsn);
    final pipeline = RumPipeline(
      transport: RumTransport(dsn: dsn, httpClient: httpClient, gzip: false),
      storage: storage ?? MemoryStorage(),
      resource: baseResource(config),
      maxBatchSize: config.maxBatchSize,
      flushInterval: config.flushInterval,
      maxQueueBytes: config.maxQueueBytes,
    );
    return TrooperClient(config: config, dsn: dsn, pipeline: pipeline, anonymousId: anonymousId);
  }
}
