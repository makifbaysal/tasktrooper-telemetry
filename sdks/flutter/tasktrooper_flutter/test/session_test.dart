import 'package:fake_async/fake_async.dart';
import 'package:flutter_test/flutter_test.dart';

import 'helpers.dart';

void main() {
  test('rotates after 30 min inactivity, emits SESSION_START', () {
    fakeAsync((async) {
      final ingest = FakeIngest();
      final c = buildClient(testConfig(flushInterval: const Duration(days: 1)), ingest);
      c.track('a');
      final first = c.session.id;
      async.elapse(const Duration(minutes: 29));
      c.track('b');
      expect(c.session.id, first);
      async.elapse(const Duration(minutes: 30, seconds: 1));
      c.track('c');
      expect(c.session.id, isNot(first));
      final events = c.pipeline.bufferedEvents;
      expect(typesOf(events), ['SESSION_START', 'CUSTOM', 'CUSTOM', 'SESSION_START', 'CUSTOM']);
      expect(events[3]['session_id'], c.session.id);
      expect(events[2]['session_id'], first);
      c.pipeline.close();
    });
  });

  test('rotates after 4 h even when active', () {
    fakeAsync((async) {
      final c = buildClient(testConfig(flushInterval: const Duration(days: 1)), FakeIngest());
      c.track('start');
      final first = c.session.id;
      for (var i = 0; i < 11; i++) {
        async.elapse(const Duration(minutes: 20));
        c.track('tick');
      }
      expect(c.session.id, first); // 3h40m
      async.elapse(const Duration(minutes: 20));
      c.track('tick');
      expect(c.session.id, isNot(first)); // 4h
      c.pipeline.close();
    });
  });

  test('active view is restarted in the new session', () {
    fakeAsync((async) {
      final c = buildClient(testConfig(flushInterval: const Duration(days: 1)), FakeIngest());
      c.startView('Home');
      final oldView = c.currentViewId;
      async.elapse(const Duration(minutes: 31));
      c.track('tap');
      expect(c.currentViewName, 'Home');
      expect(c.currentViewId, isNot(oldView));
      expect(typesOf(c.pipeline.bufferedEvents), [
        'SESSION_START',
        'VIEW_START',
        'VIEW_END',
        'SESSION_START',
        'VIEW_START',
        'CUSTOM',
      ]);
      c.pipeline.close();
    });
  });

  test('sampleRate 0 drops everything', () {
    fakeAsync((async) {
      final c = buildClient(testConfig(sampleRate: 0), FakeIngest());
      c.track('x');
      c.captureError(StateError('x'), StackTrace.current);
      expect(c.pipeline.bufferedEvents, isEmpty);
    });
  });
}
