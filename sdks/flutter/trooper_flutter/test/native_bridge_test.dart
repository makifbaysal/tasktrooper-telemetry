import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

import 'helpers.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('dev.tasktrooper.telemetry/rum');
  final messenger = TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;

  late List<MethodCall> calls;
  void mockNative(Object? Function(MethodCall call) answer) {
    calls = [];
    messenger.setMockMethodCallHandler(channel, (call) async {
      calls.add(call);
      return answer(call);
    });
  }

  tearDown(() => messenger.setMockMethodCallHandler(channel, null));

  Map<String, Object?> appStart(TrooperClient c) =>
      c.pipeline.bufferedEvents.firstWhere((e) => e['type'] == 'APP_START');

  test('channel name matches contracts §9', () {
    expect(TrooperNativeBridge.defaultChannel.name, channel.name);
  });

  test('process start from native → APP_START start_reference=process', () async {
    final processStart = DateTime.now().subtract(const Duration(milliseconds: 1500));
    mockNative((call) => call.method == 'getProcessStartTime' ? processStart.millisecondsSinceEpoch : null);
    final c = buildClient(testConfig(), FakeIngest());

    await TaskTrooper.recordColdStart(c, DateTime.now());

    expect(calls.map((c) => c.method), ['getProcessStartTime']);
    final e = appStart(c);
    expect((e['attributes'] as Map)['start_reference'], 'process');
    expect((e['app_start'] as Map)['duration_ms'] as num, greaterThanOrEqualTo(1500));
    await c.close();
  });

  test('null / missing native → start_reference=dart_init', () async {
    mockNative((_) => null);
    var c = buildClient(testConfig(), FakeIngest());
    await TaskTrooper.recordColdStart(c, DateTime.now());
    expect((appStart(c)['attributes'] as Map)['start_reference'], 'dart_init');
    await c.close();

    messenger.setMockMethodCallHandler(channel, null); // MissingPluginException
    c = buildClient(testConfig(), FakeIngest());
    await TaskTrooper.recordColdStart(c, DateTime.now());
    expect((appStart(c)['attributes'] as Map)['start_reference'], 'dart_init');
    await c.close();
  });

  test('setContext sends the shared context map', () async {
    mockNative((_) => null);
    final c = buildClient(testConfig(), FakeIngest());
    await TrooperNativeBridge(c).setContext(c.context);

    expect(calls.single.method, 'setContext');
    final args = (calls.single.arguments as Map).cast<String, Object?>();
    expect(args.keys, containsAll(['session_id', 'anonymous_id', 'view_id', 'view_name', 'user_id', 'sampled']));
    expect(args['anonymous_id'], 'test-anon');
    await c.close();
  });
}
