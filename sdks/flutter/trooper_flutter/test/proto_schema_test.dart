import 'dart:io';

import 'package:fake_async/fake_async.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

import 'helpers.dart';

/// Minimal .proto reader: message fields (name → type) and enum values.
class ProtoSchema {
  final Map<String, Map<String, String>> messages = {};
  final Map<String, Set<String>> enums = {};

  static ProtoSchema parse(String src) {
    final schema = ProtoSchema();
    final noComments = src.replaceAll(RegExp(r'//[^\n]*'), '');
    final block = RegExp(r'(message|enum)\s+(\w+)\s*\{');
    for (final m in block.allMatches(noComments)) {
      // body up to the matching brace
      var depth = 1, i = m.end;
      while (depth > 0) {
        if (noComments[i] == '{') depth++;
        if (noComments[i] == '}') depth--;
        i++;
      }
      final body = noComments.substring(m.end, i - 1);
      if (m.group(1) == 'enum') {
        schema.enums[m.group(2)!] = RegExp(r'(\w+)\s*=\s*\d+;').allMatches(body).map((e) => e.group(1)!).toSet();
      } else {
        final fields = <String, String>{};
        final field = RegExp(r'(?:repeated\s+)?(map<\s*\w+\s*,\s*\w+\s*>|[\w.]+)\s+(\w+)\s*=\s*\d+;');
        for (final f in field.allMatches(body)) {
          fields[f.group(2)!] = f.group(1)!;
        }
        schema.messages[m.group(2)!] = fields;
      }
    }
    return schema;
  }

  /// Returns violations of [json] against message [name].
  List<String> validate(Object? json, String name, [String path = r'$']) {
    final errors = <String>[];
    final fields = messages[name];
    if (fields == null) return ['unknown message $name'];
    if (json is! Map) return ['$path: expected object for $name'];
    json.forEach((key, value) {
      final type = fields[key];
      final p = '$path.$key';
      if (type == null) {
        errors.add('$p: no field "$key" in $name');
      } else if (type.startsWith('map<')) {
        if (value is! Map || value.values.any((v) => v is! String)) errors.add('$p: expected map<string,string>');
      } else if (messages.containsKey(type)) {
        if (value is List) {
          for (var i = 0; i < value.length; i++) {
            errors.addAll(validate(value[i], type, '$p[$i]'));
          }
        } else {
          errors.addAll(validate(value, type, p));
        }
      } else if (enums.containsKey(type)) {
        if (!enums[type]!.contains(value)) errors.add('$p: "$value" not in enum $type');
      } else if (type == 'string' && value is! String) {
        errors.add('$p: expected string');
      } else if (type == 'bool' && value is! bool) {
        errors.add('$p: expected bool');
      } else if ((type == 'int32' || type == 'int64') && value is! int) {
        errors.add('$p: expected int');
      } else if (type == 'double' && value is! num) {
        errors.add('$p: expected number');
      }
    });
    return errors;
  }
}

void main() {
  final schema = ProtoSchema.parse(File('../../../proto/rum/v1/rum.proto').readAsStringSync());

  test('proto parsed', () {
    expect(schema.messages['RumEvent'], containsPair('ts_ms', 'int64'));
    expect(schema.enums['EventType'], contains('VIEW_START'));
  });

  test('every event type serializes with rum.proto field names', () {
    fakeAsync((async) {
      final ingest = FakeIngest();
      final c = buildClient(testConfig(), ingest);
      c.pipeline.resource
        ..device.type = 'mobile'
        ..device.manufacturer = 'Google'
        ..device.model = 'Pixel 9'
        ..device.locale = 'en-US'
        ..device.screenWidth = 412
        ..device.screenHeight = 915
        ..osName = 'Android'
        ..osVersion = '16'
        ..attributes['team'] = 'mobile';
      c.identify('u1', traits: {'plan': 'pro'});
      c.startView('Home', attributes: {'tab': 'a'}, awaitFirstFrame: true);
      async.elapse(const Duration(milliseconds: 40));
      c.markViewLoaded(c.currentViewId!);
      c.recordFrame(buildMs: 30, rasterMs: 2, totalMs: 32);
      c.recordFrame(buildMs: 2, rasterMs: 2, totalMs: 800);
      c.recordFrame(buildMs: 2, rasterMs: 2, totalMs: 5);
      c.recordAppStart(const Duration(milliseconds: 850));
      c.captureError(StateError('boom'), StackTrace.current);
      c.recordResource(
        url: Uri.parse('https://api.shop.com/x?q=1'),
        method: 'get',
        statusCode: 200,
        duration: const Duration(milliseconds: 120),
        startedAt: DateTime.now(),
        requestBytes: 10,
        responseBytes: 2048,
        trace: TraceContext.generate(),
      );
      c.addAction('Buy', target: 'buy_button');
      c.track('checkout', props: {'cart': '3'}, value: 120.5);
      c.addVital('TTI', 300, rating: 'good');
      c.stopView();
      c.flush();
      async.flushMicrotasks();

      final batch = ingest.batches.single;
      expect(schema.validate(batch, 'RumBatch'), isEmpty);
      final types = typesOf(ingest.events).toSet();
      expect(types, schema.enums['EventType']!.difference({'EVENT_TYPE_UNSPECIFIED'}));
      expect((batch['resource'] as Map)['platform'], 'PLATFORM_FLUTTER');
      expect(((batch['resource'] as Map)['sdk'] as Map)['name'], 'trooper_flutter');

      final viewStart = ingest.events.firstWhere((e) => e['type'] == 'VIEW_START');
      expect((viewStart['view'] as Map)['loading_time_ms'], 40.0);
      expect((viewStart['view'] as Map)['is_initial'], true);
      final vitals = ingest.events.where((e) => e['type'] == 'VITAL').map((e) => e['vital'] as Map);
      expect(vitals.firstWhere((v) => v['name'] == 'SLOW_FRAMES')['value'], 1);
      expect(vitals.firstWhere((v) => v['name'] == 'FROZEN_FRAMES')['value'], 1);
      final err = ingest.events.firstWhere((e) => e['type'] == 'ERROR')['error'] as Map;
      expect(err['type'], 'StateError');
      expect(err.containsKey('is_crash'), isFalse); // false → omitted
      expect(err['stack'], isNotEmpty);
      final res = ingest.events.firstWhere((e) => e['type'] == 'RESOURCE');
      expect((res['resource'] as Map)['url'], 'https://api.shop.com/x?q');
      expect(res['user'], {
        'id': 'u1',
        'traits': {'plan': 'pro'},
      });
    });
  });

  test('beforeSend can mask and drop events', () {
    fakeAsync((async) {
      final ingest = FakeIngest();
      final c = buildClient(
        testConfig(
          beforeSend: (e) {
            if (e.type == RumEventType.custom) return null;
            final p = e.payload;
            if (p is ErrorPayload) p.message = '[masked]';
            return e;
          },
        ),
        ingest,
      );
      c.track('dropped');
      c.captureError(Exception('secret'), null);
      c.flush();
      async.flushMicrotasks();
      expect(typesOf(ingest.events), ['SESSION_START', 'ERROR']);
      expect((ingest.events.last['error'] as Map)['message'], '[masked]');
    });
  });
}
