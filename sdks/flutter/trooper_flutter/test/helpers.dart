import 'dart:convert';

import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

const testDsn = 'https://tt_test_key@ingest.example.com/7';

TrooperConfig testConfig({
  List<Pattern> targets = const [],
  double sampleRate = 1.0,
  int maxBatchSize = 100,
  int maxQueueBytes = 5 * 1024 * 1024,
  BeforeSend? beforeSend,
  Duration flushInterval = const Duration(seconds: 5),
}) => TrooperConfig(
  dsn: testDsn,
  app: 'test-app',
  version: '1.2.3',
  environment: 'test',
  sampleRate: sampleRate,
  tracePropagationTargets: targets,
  maxBatchSize: maxBatchSize,
  maxQueueBytes: maxQueueBytes,
  beforeSend: beforeSend,
  flushInterval: flushInterval,
);

/// Records uploaded batches; responds with [statuses] in order (then 200).
class FakeIngest {
  FakeIngest([List<int>? statuses, this.headers = const {}]) : statuses = statuses ?? [];

  final List<int> statuses;
  final Map<String, String> headers;
  final List<http.Request> requests = [];

  int get calls => requests.length;

  List<Map<String, Object?>> get batches => requests.map((r) => jsonDecode(r.body) as Map<String, Object?>).toList();

  List<Map<String, Object?>> get events => [
    for (final b in batches) ...(b['events'] as List).cast<Map<String, Object?>>(),
  ];

  late final http.Client client = MockClient((request) async {
    requests.add(request);
    final status = statuses.isEmpty ? 200 : statuses.removeAt(0);
    return http.Response('', status, headers: headers);
  });
}

TrooperClient buildClient(TrooperConfig config, FakeIngest ingest, {TrooperStorage? storage}) =>
    TaskTrooper.createClient(config, httpClient: ingest.client, storage: storage);

List<String> typesOf(List<Map<String, Object?>> events) => events.map((e) => e['type'] as String).toList();
