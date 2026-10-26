import 'dart:math';

import 'package:clock/clock.dart';

import 'ids.dart';

/// RUM session: rotated after [inactivityTimeout] without activity or
/// [maxDuration] in total. Sampling is decided once per session.
class SessionManager {
  SessionManager({
    this.inactivityTimeout = const Duration(minutes: 30),
    this.maxDuration = const Duration(hours: 4),
    this.sampleRate = 1.0,
    Random? random,
  }) : _random = random ?? Random();

  final Duration inactivityTimeout;
  final Duration maxDuration;
  final double sampleRate;
  final Random _random;

  String? _id;
  DateTime _startedAt = DateTime.fromMillisecondsSinceEpoch(0);
  DateTime _lastActivity = DateTime.fromMillisecondsSinceEpoch(0);
  bool _sampled = true;

  /// Current session id (null before the first [touch]).
  String? get id => _id;
  bool get sampled => _sampled;
  DateTime get startedAt => _startedAt;

  /// Whether the next activity would start a new session.
  bool get isExpired {
    if (_id == null) return true;
    final now = clock.now();
    return now.difference(_lastActivity) > inactivityTimeout || now.difference(_startedAt) >= maxDuration;
  }

  /// Starts a new session.
  void rotate() {
    final now = clock.now();
    _id = uuidV4(_random);
    _startedAt = now;
    _lastActivity = now;
    _sampled = sampleRate >= 1 || (sampleRate > 0 && _random.nextDouble() < sampleRate);
  }

  /// Records activity; returns true if a new session was started.
  bool touch() {
    if (isExpired) {
      rotate();
      return true;
    }
    _lastActivity = clock.now();
    return false;
  }
}
