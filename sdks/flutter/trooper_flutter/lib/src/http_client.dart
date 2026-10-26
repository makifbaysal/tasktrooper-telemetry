import 'package:clock/clock.dart';
import 'package:http/http.dart' as http;

import 'client.dart';
import 'task_trooper.dart';
import 'trace.dart';

/// `package:http` client that records RESOURCE events and adds W3C
/// `traceparent` and `baggage: tt.svc=<app>` headers for URLs matching
/// `tracePropagationTargets` (an existing `baggage` header is merged).
///
/// ```dart
/// final client = TrooperHttpClient();            // wraps http.Client()
/// final client = TrooperHttpClient(IOClient(...)); // or any client
/// ```
/// Headers and bodies are never recorded; query values are stripped.
/// Duration is measured until response headers arrive.
class TrooperHttpClient extends http.BaseClient {
  TrooperHttpClient([http.Client? inner, TrooperClient? client]) : _inner = inner ?? http.Client(), _client = client;

  final http.Client _inner;
  final TrooperClient? _client;

  @override
  Future<http.StreamedResponse> send(http.BaseRequest request) async {
    final c = _client ?? TaskTrooper.client;
    if (c == null) return _inner.send(request);
    TraceContext? trace;
    DateTime startedAt;
    final sw = Stopwatch()..start();
    try {
      startedAt = clock.now();
      if (c.shouldPropagate(request.url)) {
        if (!request.headers.containsKey(TraceContext.headerName)) {
          trace = TraceContext.generate();
          request.headers[TraceContext.headerName] = trace.traceparent;
        }
        try {
          final merged = Baggage.merge(request.headers[Baggage.headerName], c.config.app);
          if (merged != null) request.headers[Baggage.headerName] = merged;
        } catch (_) {}
      }
    } catch (_) {
      startedAt = DateTime.now();
    }
    try {
      final response = await _inner.send(request);
      c.recordResource(
        url: request.url,
        method: request.method,
        statusCode: response.statusCode,
        duration: sw.elapsed,
        startedAt: startedAt,
        requestBytes: request.contentLength ?? 0,
        responseBytes: response.contentLength ?? 0,
        trace: trace,
        initiator: 'http',
      );
      return response;
    } catch (_) {
      c.recordResource(
        url: request.url,
        method: request.method,
        statusCode: 0,
        duration: sw.elapsed,
        startedAt: startedAt,
        requestBytes: request.contentLength ?? 0,
        trace: trace,
        initiator: 'http',
      );
      rethrow;
    }
  }

  @override
  void close() => _inner.close();
}
