import 'dart:typed_data';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_dio/trooper_dio.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

class _FakeAdapter implements HttpClientAdapter {
  final List<RequestOptions> requests = [];

  @override
  Future<ResponseBody> fetch(
    RequestOptions options,
    Stream<Uint8List>? requestStream,
    Future<void>? cancelFuture,
  ) async {
    requests.add(options);
    if (options.uri.host == 'down.example.com') {
      throw DioException.connectionError(requestOptions: options, reason: 'offline');
    }
    final status = options.uri.path == '/missing' ? 404 : 200;
    return ResponseBody.fromString(
      '{"ok":true}',
      status,
      headers: {
        Headers.contentTypeHeader: ['application/json'],
        Headers.contentLengthHeader: ['11'],
      },
    );
  }

  @override
  void close({bool force = false}) {}
}

void main() {
  late TrooperClient client;
  late _FakeAdapter adapter;
  late Dio dio;

  setUp(() {
    client = TaskTrooper.createClient(
      const TrooperConfig(
        dsn: 'https://k@ingest.example.com/1',
        app: 'test-app',
        tracePropagationTargets: ['api.shop.com'],
      ),
    );
    adapter = _FakeAdapter();
    dio = Dio()
      ..httpClientAdapter = adapter
      ..interceptors.add(TrooperDioInterceptor(client: client));
  });

  tearDown(() => client.close());

  List<Map<String, Object?>> resources() => client.pipeline.bufferedEvents
      .where((e) => e['type'] == 'RESOURCE')
      .map((e) => (e['resource'] as Map).cast<String, Object?>())
      .toList();

  test('records success with traceparent for matching hosts', () async {
    await dio.get<dynamic>('https://api.shop.com/orders', queryParameters: {'token': 'secret'});
    await dio.get<dynamic>('https://cdn.other.com/a.png');

    final tp = adapter.requests[0].headers['traceparent'] as String;
    expect(tp, matches(RegExp(r'^00-[0-9a-f]{32}-[0-9a-f]{16}-01$')));
    expect(adapter.requests[1].headers.containsKey('traceparent'), isFalse);
    expect(adapter.requests[0].headers['baggage'], 'tt.svc=test-app');
    expect(adapter.requests[1].headers.containsKey('baggage'), isFalse);

    final r = resources();
    expect(r[0]['url'], 'https://api.shop.com/orders?token');
    expect(r[0]['method'], 'GET');
    expect(r[0]['status_code'], 200);
    expect(r[0]['response_bytes'], 11);
    expect(r[0]['initiator'], 'dio');
    expect(r[0]['trace_id'], tp.split('-')[1]);
    expect(r[1].containsKey('trace_id'), isFalse);
  });

  test('merges an existing baggage header and replaces tt.svc', () async {
    await dio.get<dynamic>(
      'https://api.shop.com/x',
      options: Options(headers: {'traceparent': 'custom', 'Baggage': 'k=v, tt.svc=old;p=1'}),
    );
    final h = adapter.requests.single.headers;
    expect(h['traceparent'], 'custom');
    final bg = h.entries.where((e) => e.key.toLowerCase() == 'baggage').toList();
    expect(bg.single.value, 'tt.svc=test-app,k=v');
  });

  test('records HTTP errors and network errors', () async {
    await expectLater(dio.get<dynamic>('https://api.shop.com/missing'), throwsA(isA<DioException>()));
    await expectLater(dio.get<dynamic>('https://down.example.com/x'), throwsA(isA<DioException>()));
    final r = resources();
    expect(r[0]['status_code'], 404);
    expect(r[1].containsKey('status_code'), isFalse); // 0 → omitted
  });

  test('keeps an existing traceparent and is a no-op without SDK', () async {
    await dio.get<dynamic>('https://api.shop.com/x', options: Options(headers: {'traceparent': 'custom'}));
    expect(adapter.requests.single.headers['traceparent'], 'custom');

    TaskTrooper.debugSetClient(null);
    final plain = Dio()
      ..httpClientAdapter = adapter
      ..interceptors.add(TrooperDioInterceptor());
    final resp = await plain.get<dynamic>('https://api.shop.com/y');
    expect(resp.statusCode, 200);
  });
}
