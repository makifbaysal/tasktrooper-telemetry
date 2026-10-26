import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

import 'helpers.dart';

void main() {
  late List<http.BaseRequest> seen;
  late http.Client inner;

  setUp(() {
    seen = [];
    inner = MockClient((r) async {
      seen.add(r);
      if (r.url.host == 'down.example.com') throw http.ClientException('offline', r.url);
      return http.Response('{"ok":true}', 201);
    });
  });

  test('adds traceparent for matching targets and records RESOURCE', () async {
    final c = buildClient(testConfig(targets: ['api.shop.com']), FakeIngest());
    final client = TrooperHttpClient(inner, c);
    await client.post(Uri.parse('https://api.shop.com/orders?token=secret'), body: 'abc');
    await client.get(Uri.parse('https://cdn.other.com/img.png'));

    final tp = seen[0].headers['traceparent']!;
    expect(tp, matches(RegExp(r'^00-[0-9a-f]{32}-[0-9a-f]{16}-01$')));
    expect(seen[1].headers.containsKey('traceparent'), isFalse);
    expect(seen[0].headers['baggage'], 'tt.svc=test-app');
    expect(seen[1].headers.containsKey('baggage'), isFalse);

    final resources = c.pipeline.bufferedEvents.where((e) => e['type'] == 'RESOURCE').toList();
    final first = resources[0]['resource'] as Map;
    expect(first['url'], 'https://api.shop.com/orders?token');
    expect(first['method'], 'POST');
    expect(first['status_code'], 201);
    expect(first['request_bytes'], 3);
    expect(first['initiator'], 'http');
    expect(first['trace_id'], tp.split('-')[1]);
    expect(first['span_id'], tp.split('-')[2]);
    expect((resources[1]['resource'] as Map).containsKey('trace_id'), isFalse);
    await c.close();
  });

  test('merges an existing baggage header and replaces tt.svc', () async {
    final c = buildClient(testConfig(targets: ['api.shop.com']), FakeIngest());
    final client = TrooperHttpClient(inner, c);
    await client.get(Uri.parse('https://api.shop.com/x'), headers: {'Baggage': 'k=v, tt.svc=old;p=1'});
    expect(seen.single.headers['baggage'], 'tt.svc=test-app,k=v');
    await c.close();
  });

  test('Baggage.merge encodes, replaces tt.svc and respects the size limit', () {
    expect(Baggage.merge(null, 'a,b=c d'), 'tt.svc=a%2Cb%3Dc%20d');
    expect(Baggage.merge('tt.svc=x, a=1 ,, b=2;prop', 'svc'), 'tt.svc=svc,a=1,b=2;prop');
    expect(Baggage.merge('a=1', ''), 'a=1');
    final big = 'k=${'x' * (Baggage.maxBytes - 5)}';
    expect(Baggage.merge(big, 'svc'), big);
  });

  test('network errors are recorded with status 0 and rethrown', () async {
    final c = buildClient(testConfig(), FakeIngest());
    final client = TrooperHttpClient(inner, c);
    await expectLater(client.get(Uri.parse('https://down.example.com/x')), throwsA(isA<http.ClientException>()));
    final r = c.pipeline.bufferedEvents.firstWhere((e) => e['type'] == 'RESOURCE')['resource'] as Map;
    expect(r.containsKey('status_code'), isFalse); // 0 → omitted
    await c.close();
  });

  test('passes through when SDK is not initialized', () async {
    TaskTrooper.debugSetClient(null);
    final client = TrooperHttpClient(inner);
    final resp = await client.get(Uri.parse('https://api.shop.com/x'));
    expect(resp.statusCode, 201);
  });
}
