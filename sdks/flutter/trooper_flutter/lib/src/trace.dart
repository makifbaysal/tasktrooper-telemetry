import 'dart:convert';
import 'dart:math';

import 'ids.dart';

/// W3C trace context for one outgoing request.
class TraceContext {
  const TraceContext(this.traceId, this.spanId);

  /// New random context (16-byte trace id, 8-byte span id).
  factory TraceContext.generate([Random? random]) => TraceContext(randomHex(16, random), randomHex(8, random));

  final String traceId;
  final String spanId;

  /// `traceparent` header value: `00-<trace-id>-<span-id>-01`.
  String get traceparent => '00-$traceId-$spanId-01';

  static const headerName = 'traceparent';
}

/// W3C `baggage` carrying the calling app name as `tt.svc=<app>`, sent next
/// to `traceparent` so backends can attribute the call (`trooper.caller`).
class Baggage {
  Baggage._();

  static const headerName = 'baggage';
  static const serviceKey = 'tt.svc';

  /// W3C limit for the whole header.
  static const maxBytes = 8192;

  /// Merges `tt.svc=<service>` (percent-encoded) into [existing], replacing
  /// any existing `tt.svc` member and keeping the others. Returns [existing]
  /// unchanged when [service] is empty, the result would exceed [maxBytes],
  /// or on any error. Never throws.
  static String? merge(String? existing, String? service) {
    try {
      if (service == null || service.isEmpty) return existing;
      final others = (existing ?? '')
          .split(',')
          .map((m) => m.trim())
          .where((m) => m.isNotEmpty && m.split(RegExp('[=;]')).first.trim() != serviceKey);
      final out = ['$serviceKey=${Uri.encodeComponent(service)}', ...others].join(',');
      return utf8.encode(out).length <= maxBytes ? out : existing;
    } catch (_) {
      return existing;
    }
  }
}

/// Whether [url] matches any of [targets]. A [RegExp] must match somewhere in
/// the full URL; a [String] matches when the URL contains it
/// (e.g. `'api.shop.com'`). Empty list = never propagate.
bool matchesPropagationTarget(Uri url, List<Pattern> targets) {
  if (targets.isEmpty) return false;
  final s = url.toString();
  for (final t in targets) {
    if (t is RegExp ? t.hasMatch(s) : t.allMatches(s).isNotEmpty) return true;
  }
  return false;
}

/// URL safe to send: no user info, no fragment, query values stripped (keys
/// kept, e.g. `?page&q`).
String sanitizeUrl(Uri url) {
  final base = Uri(
    scheme: url.scheme.isEmpty ? null : url.scheme,
    host: url.host.isEmpty ? null : url.host,
    port: url.hasPort ? url.port : null,
    path: url.path,
  ).toString();
  if (!url.hasQuery || url.query.isEmpty) return base;
  final keys = url.query.split('&').where((p) => p.isNotEmpty).map((p) => p.split('=').first).toList();
  return keys.isEmpty ? base : '$base?${keys.join('&')}';
}
