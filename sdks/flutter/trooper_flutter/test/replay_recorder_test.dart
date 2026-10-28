import 'dart:convert';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/src/replay/uploader.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

import 'helpers.dart';

void main() {
  tearDown(TrooperReplay.close);

  testWidgets('records frames and touches, uploads gzipped segments to /v1/replay', (tester) async {
    final ingest = FakeIngest();
    final client = buildClient(testConfig(), ingest);
    final storage = MemoryStorage();
    final store = MemorySegmentStore();
    client.startView('Home');

    await tester.runAsync(
      () => TrooperReplay.install(
        client,
        const TrooperReplayOptions(sampleRate: 1, captureBudget: Duration(seconds: 1)),
        storage: storage,
        segmentStore: store,
        transport: RumTransport(dsn: client.dsn, httpClient: ingest.client),
      ),
    );
    final recorder = TrooperReplay.recorder!;
    expect(TrooperReplay.isRecording, isTrue);

    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(
          body: Center(
            child: ElevatedButton(onPressed: () {}, child: const Text('Pay now')),
          ),
        ),
      ),
    );
    expect(recorder.captureNow(), isNotNull);
    await tester.tap(find.byType(ElevatedButton));
    await tester.pump();
    expect(recorder.captureNow(), isNotNull);
    expect(TrooperReplay.stats!.captures, 2);

    await tester.runAsync(TrooperReplay.flush);

    final replayRequests = ingest.requests.where((r) => r.url.path == '/v1/replay').toList();
    expect(replayRequests, hasLength(1));
    final req = replayRequests.single;
    expect(req.url.toString(), 'https://ingest.example.com/v1/replay');
    expect(req.headers['x-trooper-key'], 'tt_test_key');
    expect(req.headers['content-encoding'], 'gzip');
    final seg = jsonDecode(utf8.decode(gzip.decode(req.bodyBytes))) as Map<String, Object?>;
    expect(seg['session_id'], client.session.id);
    expect(seg['segment'], 0);
    expect(seg['format'], 'wireframe');
    expect(seg['view_name'], 'Home');
    expect((seg['resource'] as Map)['sdk_name'], 'trooper_flutter');
    expect((seg['resource'] as Map)['platform'], 'android'); // flutter_test default target platform
    final events = (seg['events'] as List).cast<Map<String, Object?>>();
    final types = events.map((e) => e['type']).toList();
    expect(types.first, 'screen');
    expect(types, containsAll(['frame', 'touch']));
    expect(events.where((e) => e['type'] == 'touch').map((e) => e['phase']), containsAllInOrder(['down', 'up']));
    final nodes = (events.firstWhere((e) => e['type'] == 'frame')['nodes'] as List).cast<Map<String, Object?>>();
    expect(nodes.any((n) => n['kind'] == 'button'), isTrue);
    expect(nodes.any((n) => n['text'] == '••• •••' && n['masked'] == true), isTrue);
    expect(jsonEncode(seg), isNot(contains('Pay now')));
    expect((seg['stats'] as Map)['clicks'], 1);

    // Sampling decision + next segment number persisted for the session.
    final state = jsonDecode((await storage.readValue('replay_state'))!) as Map<String, Object?>;
    expect(state, {'session_id': client.session.id, 'next_segment': 1, 'mode': 'record'});
    expect(await store.count(), 0);
    await TrooperReplay.close();
    await client.close();
  });

  testWidgets('on-error sessions upload only after an error', (tester) async {
    final ingest = FakeIngest();
    final client = buildClient(testConfig(), ingest);
    client.startView('Home');
    await tester.runAsync(
      () => TrooperReplay.install(
        client,
        const TrooperReplayOptions(sampleRate: 0, onErrorSampleRate: 1, captureBudget: Duration(seconds: 1)),
        segmentStore: MemorySegmentStore(),
        transport: RumTransport(dsn: client.dsn, httpClient: ingest.client),
      ),
    );
    await tester.pumpWidget(const MaterialApp(home: Text('x')));
    TrooperReplay.recorder!.captureNow();
    await tester.runAsync(TrooperReplay.flush);
    expect(ingest.requests.where((r) => r.url.path == '/v1/replay'), isEmpty);
    expect(TrooperReplay.isRecording, isFalse);

    client.captureError(StateError('boom'), StackTrace.current);
    expect(TrooperReplay.isRecording, isTrue);
    await tester.runAsync(TrooperReplay.flush);
    final segs = ingest.requests.where((r) => r.url.path == '/v1/replay').toList();
    expect(segs, hasLength(1));
    final seg = jsonDecode(utf8.decode(gzip.decode(segs.single.bodyBytes))) as Map<String, Object?>;
    expect((seg['stats'] as Map)['errors'], 1);
    expect(seg['has_full_snapshot'], isTrue);
    await TrooperReplay.close();
    await client.close();
  });

  testWidgets('RUM-unsampled sessions are never recorded', (tester) async {
    final ingest = FakeIngest();
    final client = buildClient(testConfig(sampleRate: 0), ingest);
    client.startView('Home');
    await tester.runAsync(
      () => TrooperReplay.install(
        client,
        const TrooperReplayOptions(sampleRate: 1, captureBudget: Duration(seconds: 1)),
        segmentStore: MemorySegmentStore(),
        transport: RumTransport(dsn: client.dsn, httpClient: ingest.client),
      ),
    );
    await tester.pumpWidget(const MaterialApp(home: Text('x')));
    expect(TrooperReplay.recorder!.engine.mode, ReplayMode.off);
    await TrooperReplay.close();
    await client.close();
  });

  test('uploader: 5xx retries with backoff, 4xx drops', () async {
    final ingest = FakeIngest([503, 400]);
    final client = buildClient(testConfig(), ingest);
    final store = MemorySegmentStore();
    final up = ReplayUploader(
      store: store,
      transport: RumTransport(dsn: client.dsn, httpClient: ingest.client),
      url: client.dsn.replayEndpoint,
      gzipped: false,
    );
    await up.enqueue([1]);
    expect(up.isBackingOff, isTrue);
    expect(await store.count(), 1);
    up.close();
    final up2 = ReplayUploader(
      store: store,
      transport: RumTransport(dsn: client.dsn, httpClient: ingest.client),
      url: client.dsn.replayEndpoint,
      gzipped: false,
    );
    await up2.drain();
    expect(up2.rejected, 1);
    expect(await store.count(), 0);
  });

  test('segment store drops new segments beyond the cap', () async {
    final store = MemorySegmentStore(maxBytes: 10);
    expect(await store.add(List.filled(8, 0)), isTrue);
    expect(await store.add(List.filled(8, 0)), isFalse);
    expect(store.dropped, 1);
  });
}
