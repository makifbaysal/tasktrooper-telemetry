import 'dart:convert';
import 'dart:math';

import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/src/replay/engine.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

class _Harness {
  _Harness({
    TrooperReplayOptions options = const TrooperReplayOptions(sampleRate: 1),
    ReplayState? restored,
    bool sampled = true,
    Random? random,
  }) {
    engine = ReplayEngine(
      options: options,
      now: () => now,
      context: () => sessionId == null
          ? null
          : ReplayContext(
              sessionId: sessionId!,
              sampled: sampled,
              anonymousId: 'anon-1',
              viewId: 'v1',
              viewName: viewName,
              userId: 'u1',
            ),
      resource: () =>
          const ReplayResource(service: 'app', env: 'test', version: '1.0', platform: 'ios', sdkVersion: '0.1.0'),
      sink: (json, seg) => segments.add(jsonDecode(json) as Map<String, Object?>),
      persist: (s) => persisted.add(s),
      restored: restored,
      random: random,
    );
  }

  late final ReplayEngine engine;
  int now = 1000000;
  String? sessionId = 's1';
  String viewName = 'Home';
  final segments = <Map<String, Object?>>[];
  final persisted = <ReplayState>[];

  void frame(String nodes) => engine.onFrame(now, 390, 844, 3, nodes, keyboardVisible: false);
}

List<String> _types(Map<String, Object?> seg) =>
    (seg['events'] as List).map((e) => (e as Map<String, Object?>)['type'] as String).toList();

void main() {
  test('segment shape: resource, ids, screen first, frames dedup, touches, stats', () {
    final h = _Harness();
    h.frame('[{"id":1}]');
    h.now += 100;
    h.frame('[{"id":1}]'); // unchanged → not recorded
    h.engine.onTouch(h.now, 10.04, 20, 'down');
    h.engine.onTouch(h.now + 50, 10, 20, 'up');
    h.now += 600;
    h.frame('[{"id":2}]');
    h.engine.flush();

    expect(h.segments, hasLength(1));
    final s = h.segments.single;
    expect(s['resource'], {
      'service': 'app',
      'env': 'test',
      'version': '1.0',
      'platform': 'ios',
      'sdk_name': 'trooper_flutter',
      'sdk_version': '0.1.0',
    });
    expect(s['session_id'], 's1');
    expect(s['segment'], 0);
    expect(s['format'], 'wireframe');
    expect(s['has_full_snapshot'], true);
    expect(s['view_name'], 'Home');
    expect(s['view_id'], 'v1');
    expect(s['user_id'], 'u1');
    expect(s['anonymous_id'], 'anon-1');
    expect(s['stats'], {'clicks': 1, 'errors': 0});
    expect(s['start_ts'], 1000000);
    expect(s['end_ts'], 1000700);
    expect(_types(s), ['screen', 'keyboard', 'frame', 'touch', 'touch', 'frame']);
    final events = (s['events'] as List).cast<Map<String, Object?>>();
    expect(events.first, {'type': 'screen', 'ts': 1000000, 'width': 390, 'height': 844, 'density': 3, 'name': 'Home'});
    expect(events[3], {'type': 'touch', 'ts': 1000100, 'x': 10, 'y': 20, 'phase': 'down'});
    expect(events[2]['nodes'], [
      {'id': 1},
    ]);
  });

  test('5 s segment boundary; every segment starts with screen; counter increments and persists', () {
    final h = _Harness();
    for (var i = 0; i < 12; i++) {
      h.frame('[{"id":$i}]');
      h.now += 1000;
      h.engine.tick();
    }
    expect(h.segments.length, 2);
    expect(h.segments.map((s) => s['segment']), [0, 1]);
    for (final s in h.segments) {
      expect(_types(s).first, 'screen');
      expect((s['end_ts'] as int) - (s['start_ts'] as int), lessThan(5000));
    }
    expect(h.persisted.last.segment, 2);
    expect(h.persisted.last.sessionId, 's1');
    expect(h.persisted.last.mode, ReplayMode.record);
  });

  test('256 KB size boundary', () {
    final h = _Harness();
    final big = '[{"text":"${'x' * 60000}"}]';
    for (var i = 0; i < 6; i++) {
      h.frame(big.replaceFirst('x', '$i'));
    }
    expect(h.segments, isNotEmpty);
    expect(utf8.encode(jsonEncode(h.segments.first)).length, lessThan(ReplayEngine.segmentMaxBytes + 70000));
  });

  test('a new view name emits a new screen event', () {
    final h = _Harness();
    h.frame('[]');
    h.viewName = 'Product';
    h.now += 500;
    h.frame('[]');
    h.engine.flush();
    final events = (h.segments.single['events'] as List).cast<Map<String, Object?>>();
    expect(events.where((e) => e['type'] == 'screen').map((e) => e['name']), ['Home', 'Product']);
    expect(events.where((e) => e['type'] == 'frame'), hasLength(2));
  });

  test('sampling: record / buffer / off, decided once per session', () {
    expect(_Harness(options: const TrooperReplayOptions(sampleRate: 1)).engine.refreshSession(), ReplayMode.record);
    expect(
      _Harness(options: const TrooperReplayOptions(sampleRate: 0, onErrorSampleRate: 1)).engine.refreshSession(),
      ReplayMode.buffer,
    );
    expect(
      _Harness(options: const TrooperReplayOptions(sampleRate: 0, onErrorSampleRate: 0)).engine.refreshSession(),
      ReplayMode.off,
    );
    expect(_Harness(sampled: false).engine.refreshSession(), ReplayMode.off, reason: 'RUM-unsampled session');
    expect(
      _Harness(options: const TrooperReplayOptions(sampleRate: 1, enabled: false)).engine.refreshSession(),
      ReplayMode.off,
    );

    // ~10 % of sessions.
    final h = _Harness(options: const TrooperReplayOptions(sampleRate: 0.1, onErrorSampleRate: 0), random: Random(1));
    var recorded = 0;
    for (var i = 0; i < 1000; i++) {
      h.sessionId = 's$i';
      if (h.engine.refreshSession() == ReplayMode.record) recorded++;
      h.engine.refreshSession(); // same session: no new decision
    }
    expect(recorded, inInclusiveRange(70, 130));
  });

  test('no session yet → off, starts once the RUM session exists', () {
    final h = _Harness()..sessionId = null;
    h.frame('[]');
    expect(h.engine.mode, ReplayMode.off);
    h.sessionId = 's1';
    h.frame('[]');
    expect(h.engine.mode, ReplayMode.record);
  });

  test('restored state continues the same session numbering and decision', () {
    final h = _Harness(
      options: const TrooperReplayOptions(sampleRate: 0, onErrorSampleRate: 0),
      restored: const ReplayState('s1', 7, ReplayMode.record),
    );
    h.frame('[]');
    h.engine.flush();
    expect(h.engine.mode, ReplayMode.record);
    expect(h.segments.single['segment'], 7);

    final other = _Harness(restored: const ReplayState('old', 7, ReplayMode.record));
    other.frame('[]');
    other.engine.flush();
    expect(other.segments.single['segment'], 0);
    expect(ReplayState.decode(const ReplayState('s', 3, ReplayMode.buffer).encode())?.segment, 3);
  });

  test('on-error buffer keeps ~60 s, flushes on error and keeps recording', () {
    final h = _Harness(options: const TrooperReplayOptions(sampleRate: 0, onErrorSampleRate: 1));
    for (var i = 0; i < 120; i++) {
      h.frame('[{"id":$i}]');
      h.now += 1000;
      h.engine.tick();
    }
    expect(h.segments, isEmpty, reason: 'buffer mode uploads nothing before an error');
    h.engine.onError(h.now);
    expect(h.engine.mode, ReplayMode.record);
    expect(h.segments, isNotEmpty);
    final first = h.segments.first;
    expect(_types(first).first, 'screen');
    expect(_types(first)[1], anyOf('frame', 'keyboard'));
    final span = (h.segments.last['end_ts'] as int) - (first['start_ts'] as int);
    expect(span, inInclusiveRange(59000, 61000));
    expect(h.segments.map((s) => (s['stats'] as Map)['errors']).reduce((a, b) => (a as int) + (b as int)), 1);
    // Recording continues.
    h.frame('[{"id":"after"}]');
    h.now += 6000;
    h.engine.tick();
    expect(jsonEncode(h.segments.last), contains('after'));
  });

  test('session rotation seals the old session and restarts numbering', () {
    final h = _Harness();
    h.frame('[]');
    h.sessionId = 's2';
    h.now += 100;
    h.frame('[]');
    h.engine.flush();
    expect(h.segments.map((s) => '${s['session_id']}/${s['segment']}'), ['s1/0', 's2/0']);
  });
}
