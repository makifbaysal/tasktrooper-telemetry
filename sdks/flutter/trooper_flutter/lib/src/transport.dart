import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:http/http.dart' as http;

import 'dsn.dart';
import 'platform/platform.dart';

/// Outcome of one batch upload.
sealed class SendResult {
  const SendResult();
}

class SendOk extends SendResult {
  const SendOk();
}

/// Non-retryable (4xx except 408/429): the batch is discarded.
class SendDrop extends SendResult {
  const SendDrop(this.statusCode);
  final int statusCode;
}

/// Retry later (429, 5xx, 408, network error / timeout).
class SendRetry extends SendResult {
  const SendRetry([this.retryAfter]);
  final Duration? retryAfter;
}

/// POSTs RUM batches to `/v1/rum` (contracts.md §2).
class RumTransport {
  RumTransport({
    required this.dsn,
    http.Client? httpClient,
    this.gzip = true,
    this.timeout = const Duration(seconds: 30),
  }) : _http = httpClient ?? http.Client();

  final Dsn dsn;
  final bool gzip;
  final Duration timeout;
  final http.Client _http;

  Future<SendResult> send(String jsonBody) async {
    try {
      final raw = utf8.encode(jsonBody);
      final compressed = gzip ? gzipBytes(raw) : null;
      final headers = <String, String>{
        'content-type': 'application/json',
        'x-trooper-key': dsn.key,
        if (compressed != null) 'content-encoding': 'gzip',
      };
      final resp = await _http.post(dsn.endpoint, headers: headers, body: compressed ?? raw).timeout(timeout);
      final code = resp.statusCode;
      if (code >= 200 && code < 300) return const SendOk();
      if (code == 429 || code == 408 || code >= 500) {
        return SendRetry(parseRetryAfter(resp.headers['retry-after']));
      }
      return SendDrop(code);
    } catch (_) {
      return const SendRetry();
    }
  }

  void close() {
    try {
      _http.close();
    } catch (_) {}
  }
}

/// `Retry-After` in seconds (HTTP-date form is ignored), capped at 10 min.
Duration? parseRetryAfter(String? value) {
  final s = int.tryParse(value?.trim() ?? '');
  if (s == null || s < 0) return null;
  return Duration(seconds: min(s, 600));
}

/// Exponential backoff 1 s → 60 s with jitter in [50 %, 100 %] of the step.
Duration backoffDelay(int attempt, [Random? random]) {
  final r = random ?? Random();
  final stepMs = min(60000, 1000 * pow(2, min(attempt, 16)).toInt());
  final ms = (stepMs * (0.5 + r.nextDouble() * 0.5)).round();
  return Duration(milliseconds: ms);
}
