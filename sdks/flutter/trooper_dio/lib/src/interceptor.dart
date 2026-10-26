import 'package:clock/clock.dart';
import 'package:dio/dio.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

const _kStart = '_trooper.start';
const _kWatch = '_trooper.watch';
const _kTrace = '_trooper.trace';

/// Records every Dio request as a RESOURCE event (`initiator: dio`) and adds
/// W3C `traceparent` and `baggage: tt.svc=<app>` headers for URLs matching
/// `tracePropagationTargets` (an existing `baggage` header is merged).
///
/// ```dart
/// final dio = Dio()..interceptors.add(TrooperDioInterceptor());
/// ```
/// Add it first so the measured duration covers the other interceptors.
/// Headers, bodies and query values are never recorded.
class TrooperDioInterceptor extends Interceptor {
  TrooperDioInterceptor({TrooperClient? client}) : _client = client;

  final TrooperClient? _client;

  TrooperClient? get _c => _client ?? TaskTrooper.client;

  @override
  void onRequest(RequestOptions options, RequestInterceptorHandler handler) {
    try {
      final c = _c;
      if (c != null && !c.isIngestRequest(options.uri)) {
        options.extra[_kStart] = clock.now();
        options.extra[_kWatch] = Stopwatch()..start();
        final hasHeader = options.headers.keys.any((k) => k.toLowerCase() == TraceContext.headerName);
        if (c.shouldPropagate(options.uri)) {
          if (!hasHeader) {
            final trace = TraceContext.generate();
            options.headers[TraceContext.headerName] = trace.traceparent;
            options.extra[_kTrace] = trace;
          }
          _setBaggage(options.headers, c.config.app);
        }
      }
    } catch (_) {}
    handler.next(options);
  }

  static void _setBaggage(Map<String, dynamic> headers, String app) {
    try {
      final keys = headers.keys.where((k) => k.toLowerCase() == Baggage.headerName).toList();
      final existing = keys
          .map((k) => headers[k])
          .expand((v) => v is Iterable ? v : [v])
          .where((v) => v != null)
          .join(',');
      final merged = Baggage.merge(existing.isEmpty ? null : existing, app);
      if (merged == null) return;
      for (final k in keys) {
        headers.remove(k);
      }
      headers[Baggage.headerName] = merged;
    } catch (_) {}
  }

  @override
  void onResponse(Response<dynamic> response, ResponseInterceptorHandler handler) {
    _record(response.requestOptions, response.statusCode ?? 0, response.headers);
    handler.next(response);
  }

  @override
  void onError(DioException err, ErrorInterceptorHandler handler) {
    _record(err.requestOptions, err.response?.statusCode ?? 0, err.response?.headers);
    handler.next(err);
  }

  void _record(RequestOptions options, int status, Headers? headers) {
    try {
      final c = _c;
      final watch = options.extra.remove(_kWatch);
      final started = options.extra.remove(_kStart);
      final trace = options.extra.remove(_kTrace);
      if (c == null || watch is! Stopwatch || started is! DateTime) return;
      c.recordResource(
        url: options.uri,
        method: options.method,
        statusCode: status,
        duration: watch.elapsed,
        startedAt: started,
        requestBytes: _requestBytes(options),
        responseBytes: int.tryParse(headers?.value(Headers.contentLengthHeader) ?? '') ?? 0,
        trace: trace is TraceContext ? trace : null,
        initiator: 'dio',
      );
    } catch (_) {}
  }

  static int _requestBytes(RequestOptions options) {
    final header = options.headers[Headers.contentLengthHeader];
    if (header != null) return int.tryParse('$header') ?? 0;
    final data = options.data;
    if (data is List<int>) return data.length;
    if (data is String) return data.length;
    return 0;
  }
}
