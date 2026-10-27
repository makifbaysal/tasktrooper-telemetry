/// Parsed DSN: `https://<ingest-key>@<host>[:port]/<project_id>`.
///
/// A path prefix before the project id is kept (reverse proxies), so
/// `https://k@example.com/telemetry/42` posts to `https://example.com/telemetry/v1/rum`.
class Dsn {
  const Dsn._(this.endpoint, this.key, this.projectId);

  /// Full RUM ingest URL (`.../v1/rum`).
  final Uri endpoint;

  /// Ingest key, sent as `x-trooper-key`.
  final String key;
  final String projectId;

  /// Session replay ingest URL (`.../v1/replay`, contracts.md §23).
  Uri get replayEndpoint =>
      endpoint.replace(pathSegments: [...endpoint.pathSegments.sublist(0, endpoint.pathSegments.length - 1), 'replay']);

  /// Throws [FormatException] for an invalid DSN.
  static Dsn parse(String dsn) {
    final uri = Uri.parse(dsn.trim());
    if (uri.scheme != 'http' && uri.scheme != 'https') {
      throw FormatException('DSN scheme must be http or https', dsn);
    }
    if (uri.host.isEmpty) throw FormatException('DSN has no host', dsn);
    final key = Uri.decodeComponent(uri.userInfo.split(':').first);
    if (key.isEmpty) throw FormatException('DSN has no ingest key', dsn);
    final segments = uri.pathSegments.where((s) => s.isNotEmpty).toList();
    if (segments.isEmpty) throw FormatException('DSN has no project id', dsn);
    final endpoint = Uri(
      scheme: uri.scheme,
      host: uri.host,
      port: uri.hasPort ? uri.port : null,
      pathSegments: [...segments.sublist(0, segments.length - 1), 'v1', 'rum'],
    );
    return Dsn._(endpoint, key, segments.last);
  }

  @override
  String toString() => 'Dsn($endpoint, project: $projectId)';
}
