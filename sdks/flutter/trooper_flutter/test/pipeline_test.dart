import 'dart:convert';

import 'package:fake_async/fake_async.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

import 'helpers.dart';

void main() {
  group('batching', () {
    test('flushes after 5 s', () {
      fakeAsync((async) {
        final ingest = FakeIngest();
        final c = buildClient(testConfig(), ingest);
        c.track('a');
        c.track('b');
        async.elapse(const Duration(seconds: 4));
        expect(ingest.calls, 0);
        async.elapse(const Duration(seconds: 1));
        expect(ingest.calls, 1);
        expect(typesOf(ingest.events), ['SESSION_START', 'CUSTOM', 'CUSTOM']);
        final r = ingest.requests.single;
        expect(r.url.toString(), 'https://ingest.example.com/v1/rum');
        expect(r.headers['x-trooper-key'], 'tt_test_key');
        expect(r.headers['content-type'], startsWith('application/json'));
      });
    });

    test('flushes immediately at 100 events, batches never exceed 100', () {
      fakeAsync((async) {
        final ingest = FakeIngest();
        final c = buildClient(testConfig(), ingest);
        for (var i = 0; i < 249; i++) {
          c.track('e$i'); // + 1 SESSION_START = 250 events
        }
        async.flushMicrotasks();
        expect(ingest.calls, 2);
        expect(ingest.batches.map((b) => (b['events'] as List).length), [100, 100]);
        async.elapse(const Duration(seconds: 5));
        expect(ingest.batches.map((b) => (b['events'] as List).length), [100, 100, 50]);
      });
    });

    test('explicit flush (background) uploads right away', () {
      fakeAsync((async) {
        final ingest = FakeIngest();
        final c = buildClient(testConfig(), ingest);
        c.startView('Home');
        c.onBackground();
        async.flushMicrotasks();
        expect(ingest.calls, 1);
        expect(typesOf(ingest.events), ['SESSION_START', 'VIEW_START', 'VIEW_END']);
      });
    });
  });

  group('retry / backoff', () {
    test('5xx retries with exponential backoff, then succeeds', () {
      fakeAsync((async) {
        final ingest = FakeIngest([503, 500, 200]);
        final c = buildClient(testConfig(), ingest);
        c.track('a');
        c.flush();
        async.flushMicrotasks();
        expect(ingest.calls, 1);
        expect(c.pipeline.isBackingOff, isTrue);
        async.elapse(const Duration(milliseconds: 499));
        expect(ingest.calls, 1); // step 1 s, jitter 0.5–1 s
        async.elapse(const Duration(milliseconds: 501));
        expect(ingest.calls, 2);
        async.elapse(const Duration(seconds: 2)); // step 2 s, jitter 1–2 s
        expect(ingest.calls, 3);
        expect(c.pipeline.queuedBatches, isEmpty);
        expect(c.pipeline.isBackingOff, isFalse);
        // all three attempts carried the same batch
        expect(ingest.requests.map((r) => r.body).toSet().length, 1);
      });
    });

    test('429 honours Retry-After', () {
      fakeAsync((async) {
        final ingest = FakeIngest([429], {'retry-after': '7'});
        final c = buildClient(testConfig(), ingest);
        c.track('a');
        c.flush();
        async.flushMicrotasks();
        async.elapse(const Duration(seconds: 6));
        expect(ingest.calls, 1);
        async.elapse(const Duration(seconds: 1));
        expect(ingest.calls, 2);
      });
    });

    test('4xx drops the batch and continues', () {
      fakeAsync((async) {
        final ingest = FakeIngest([400, 200]);
        final c = buildClient(testConfig(maxBatchSize: 2), ingest);
        c.track('a'); // SESSION_START + a → batch 1
        c.track('b');
        c.track('c'); // batch 2
        async.flushMicrotasks();
        expect(ingest.calls, 2);
        expect(c.pipeline.queuedBatches, isEmpty);
        expect(c.pipeline.isBackingOff, isFalse);
      });
    });

    test('backoff is capped at 60 s with jitter', () {
      for (var attempt = 0; attempt < 20; attempt++) {
        final d = backoffDelay(attempt);
        final step = attempt >= 6 ? 60000 : 1000 * (1 << attempt);
        expect(d.inMilliseconds, inInclusiveRange(step ~/ 2, step));
      }
      expect(parseRetryAfter('12'), const Duration(seconds: 12));
      expect(parseRetryAfter('Wed, 21 Oct 2015 07:28:00 GMT'), isNull);
    });
  });

  group('offline queue', () {
    test('keeps failed batches on storage and caps size FIFO', () {
      fakeAsync((async) {
        final ingest = FakeIngest(List.filled(1000, 503));
        final storage = MemoryStorage();
        final c = buildClient(testConfig(maxBatchSize: 10, maxQueueBytes: 8 * 1024), ingest, storage: storage);
        for (var i = 0; i < 300; i++) {
          c.track('event-$i', props: {'i': '$i'});
        }
        c.flush();
        async.flushMicrotasks();
        final queued = c.pipeline.queuedBatches;
        final bytes = queued.fold<int>(0, (s, b) => s + utf8.encode(b).length);
        expect(bytes, lessThanOrEqualTo(8 * 1024));
        expect(queued.length, lessThan(30)); // oldest dropped
        expect(queued.last, contains('event-299'));
        expect(storage.queue, queued);
        c.pipeline.close();
      });
    });

    test('restores persisted batches and uploads them', () {
      fakeAsync((async) {
        final storage = MemoryStorage()..queue = ['{"resource":{},"events":[]}'];
        final ingest = FakeIngest();
        final c = buildClient(testConfig(), ingest, storage: storage);
        c.pipeline.restore();
        async.flushMicrotasks();
        expect(ingest.calls, 1);
        expect(storage.queue, isEmpty);
      });
    });
  });
}
