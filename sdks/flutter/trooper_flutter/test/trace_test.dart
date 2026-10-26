import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

void main() {
  test('traceparent format', () {
    final tc = TraceContext.generate();
    expect(tc.traceparent, matches(RegExp(r'^00-[0-9a-f]{32}-[0-9a-f]{16}-01$')));
    expect(tc.traceId, isNot('0' * 32));
    expect(TraceContext.generate().traceId, isNot(tc.traceId));
  });

  test('target matching', () {
    final api = Uri.parse('https://api.shop.com/v1/orders?id=1');
    expect(matchesPropagationTarget(api, const []), isFalse);
    expect(matchesPropagationTarget(api, ['api.shop.com']), isTrue);
    expect(matchesPropagationTarget(api, ['cdn.shop.com']), isFalse);
    expect(matchesPropagationTarget(api, [RegExp(r'^https://api\.shop\.com/')]), isTrue);
    expect(
      matchesPropagationTarget(Uri.parse('https://evil.com/?u=https://api.shop.com/'), [
        RegExp(r'^https://api\.shop\.com/'),
      ]),
      isFalse,
    );
  });

  test('sanitizeUrl strips query values, userinfo and fragment', () {
    expect(
      sanitizeUrl(Uri.parse('https://u:p@api.shop.com:8443/a/b?token=secret&page=2#frag')),
      'https://api.shop.com:8443/a/b?token&page',
    );
    expect(sanitizeUrl(Uri.parse('https://api.shop.com/a')), 'https://api.shop.com/a');
  });

  test('client never propagates to the ingest endpoint', () {
    final c = TaskTrooper.createClient(
      TrooperConfig(dsn: 'https://k@ingest.example.com/1', app: 'a', tracePropagationTargets: [RegExp('.*')]),
    );
    expect(c.shouldPropagate(Uri.parse('https://ingest.example.com/v1/rum')), isFalse);
    expect(c.shouldPropagate(Uri.parse('https://api.example.com/x')), isTrue);
  });
}
