import 'model.dart';

/// Mutate or drop (return null) an event before it is queued. Use it to mask
/// PII (URLs, error messages). Exceptions thrown here drop the event.
typedef BeforeSend = RumEvent? Function(RumEvent event);

/// SDK configuration. Only [dsn] and [app] are required.
class TrooperConfig {
  const TrooperConfig({
    required this.dsn,
    required this.app,
    this.version = '',
    this.environment = 'production',
    this.sampleRate = 1.0,
    this.tracePropagationTargets = const [],
    this.beforeSend,
    this.attributes = const {},
    this.captureErrors = true,
    this.trackFrames = true,
    this.trackAppStart = true,
    this.trackLifecycle = true,
    this.nativeBridge = true,
    this.flushInterval = const Duration(seconds: 5),
    this.maxBatchSize = 100,
    this.maxQueueBytes = 5 * 1024 * 1024,
    this.sessionInactivityTimeout = const Duration(minutes: 30),
    this.sessionMaxDuration = const Duration(hours: 4),
    this.debug = false,
  });

  /// `https://<ingest-key>@<host>[:port]/<project_id>`.
  final String dsn;

  /// Service name of the app as shown in the dashboard, e.g. `courier-app`.
  final String app;

  /// App version / build (deploy marker).
  final String version;
  final String environment;

  /// Fraction of sessions recorded, 0.0–1.0 (decided per session).
  final double sampleRate;

  /// URLs that receive W3C `traceparent` and `baggage: tt.svc=<app>` headers. [RegExp]s match anywhere in
  /// the URL, [String]s match as substrings. Empty = no propagation.
  final List<Pattern> tracePropagationTargets;
  final BeforeSend? beforeSend;

  /// Extra `Resource.attributes`.
  final Map<String, String> attributes;

  /// Install `FlutterError.onError`, `PlatformDispatcher.onError` handlers.
  final bool captureErrors;

  /// Slow / frozen frame vitals per view.
  final bool trackFrames;

  /// APP_START event (init → first rasterized frame, or native process start).
  final bool trackAppStart;

  /// Flush and end the view when the app goes to background.
  final bool trackLifecycle;

  /// Share session/view context with the native Android/iOS SDKs over a
  /// MethodChannel (no-op if they are not installed).
  final bool nativeBridge;
  final Duration flushInterval;
  final int maxBatchSize;
  final int maxQueueBytes;
  final Duration sessionInactivityTimeout;
  final Duration sessionMaxDuration;

  /// Print internal SDK errors with `debugPrint`.
  final bool debug;
}
