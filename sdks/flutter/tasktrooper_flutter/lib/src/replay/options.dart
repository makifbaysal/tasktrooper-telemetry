/// Session replay options (contracts.md §23, ADR-021). Flutter replay is a **wireframe** built from the Element
/// tree in Dart: no screenshots or pixels are ever captured.
class TrooperReplayOptions {
  const TrooperReplayOptions({
    this.sampleRate = 0.1,
    this.onErrorSampleRate = 1.0,
    this.maskAllText = true,
    this.enabled = true,
    this.maxQueueBytes = 2 * 1024 * 1024,
    this.minCaptureInterval = const Duration(milliseconds: 500),
    this.maxNodes = 1500,
    this.captureBudget = const Duration(milliseconds: 8),
  });

  /// Fraction of (RUM-sampled) sessions recorded from the start. Decided once per session.
  final double sampleRate;

  /// Fraction of the remaining sessions that keep the last 60 s in memory and start uploading (and keep
  /// recording) when an ERROR is recorded.
  final double onErrorSampleRate;

  /// Mask all text (length kept as `•`). Text fields are always masked; unmask subtrees with
  /// `TrooperUnmask(child: …)`.
  final bool maskAllText;

  /// `false` turns replay off entirely.
  final bool enabled;

  /// Offline cap for unsent segments; newer segments are dropped beyond it.
  final int maxQueueBytes;

  /// Minimum time between two captures (default 500 ms = 2 fps). Raised automatically when captures are slow.
  final Duration minCaptureInterval;

  /// A capture stops after this many nodes (frame is still recorded, marked as truncated internally).
  final int maxNodes;

  /// A capture stops after this much main-isolate time.
  final Duration captureBudget;
}
