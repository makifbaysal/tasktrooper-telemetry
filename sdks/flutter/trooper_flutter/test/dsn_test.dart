import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

void main() {
  test('parses https DSN', () {
    final d = Dsn.parse('https://abc123@ingest.trooper.dev/42');
    expect(d.key, 'abc123');
    expect(d.projectId, '42');
    expect(d.endpoint.toString(), 'https://ingest.trooper.dev/v1/rum');
  });

  test('keeps port and path prefix', () {
    final d = Dsn.parse('http://tt_dev_ingest_key@localhost:4318/1');
    expect(d.endpoint.toString(), 'http://localhost:4318/v1/rum');
    expect(d.key, 'tt_dev_ingest_key');
    final p = Dsn.parse(' https://k@proxy.example.com/telemetry/9 ');
    expect(p.endpoint.toString(), 'https://proxy.example.com/telemetry/v1/rum');
    expect(p.projectId, '9');
  });

  test('rejects invalid DSNs', () {
    for (final bad in ['', 'ftp://k@h/1', 'https://h/1', 'https://k@h', 'https://k@h/', 'not a url']) {
      expect(() => Dsn.parse(bad), throwsFormatException, reason: bad);
    }
  });
}
