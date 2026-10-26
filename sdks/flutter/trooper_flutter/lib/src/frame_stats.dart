/// Slow / frozen frame counters for one view.
///
/// A frame is *frozen* when its total span exceeds [frozenThresholdMs]
/// (700 ms), otherwise *slow* when build or raster exceeds [slowThresholdMs]
/// (16 ms, i.e. a missed 60 Hz deadline).
class FrameStats {
  static const double slowThresholdMs = 16;
  static const double frozenThresholdMs = 700;

  int total = 0;
  int slow = 0;
  int frozen = 0;

  void add({required double buildMs, required double rasterMs, required double totalMs}) {
    total++;
    if (totalMs > frozenThresholdMs) {
      frozen++;
    } else if (buildMs > slowThresholdMs || rasterMs > slowThresholdMs) {
      slow++;
    }
  }

  /// good ≤ 5 % slow frames, needs-improvement ≤ 10 %, else poor.
  String get slowRating {
    if (total == 0) return 'good';
    final ratio = slow / total;
    return ratio <= 0.05 ? 'good' : (ratio <= 0.10 ? 'needs-improvement' : 'poor');
  }

  /// good = none, needs-improvement = 1, poor = more.
  String get frozenRating => frozen == 0 ? 'good' : (frozen == 1 ? 'needs-improvement' : 'poor');
}
