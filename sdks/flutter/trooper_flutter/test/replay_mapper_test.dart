import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/src/replay/mapper.dart';
import 'package:trooper_flutter/src/replay/wireframe.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

final _secret = TextEditingController(text: 'hunter2');

Widget _app() => MaterialApp(
  home: Scaffold(
    appBar: AppBar(title: const Text('Checkout')),
    body: Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [
        const Text('Hello world'),
        const TrooperUnmask(child: Text('Order total')),
        TextField(controller: _secret, obscureText: true),
        ElevatedButton(onPressed: () {}, child: const Text('Buy')),
        const Icon(Icons.add, color: Color(0xff112233)),
        Switch(value: true, onChanged: (_) {}),
        Checkbox(value: false, onChanged: (_) {}),
        Container(
          width: 40,
          height: 20,
          decoration: BoxDecoration(
            color: const Color(0xffff0000),
            borderRadius: BorderRadius.circular(6),
            border: Border.all(color: const Color(0xff00ff00)),
          ),
        ),
        const Card(color: Color(0xff0000ff), child: SizedBox(width: 30, height: 30)),
        const Opacity(opacity: 0, child: Text('Invisible')),
        const Offstage(child: Text('Offstage hidden')),
        SizedBox(height: 50, child: ListView(children: [for (var i = 0; i < 50; i++) Text('Row $i')])),
      ],
    ),
  ),
);

List<WireNode> _capture(WidgetTester tester, {bool maskAllText = true}) {
  final view = tester.view;
  final size = view.physicalSize / view.devicePixelRatio;
  return ReplayMapper(
    maskAllText: maskAllText,
    budget: const Duration(seconds: 1),
  ).capture(tester.binding.rootElement!, size).nodes;
}

void main() {
  testWidgets('maps widgets to wireframe kinds with masking', (tester) async {
    await tester.pumpWidget(_app());
    final nodes = _capture(tester);
    WireNode? byText(String t) => nodes.where((n) => n.text == t).firstOrNull;

    // Text masked, length and spaces preserved.
    final hello = byText('••••• •••••');
    expect(hello, isNotNull);
    expect(hello!.kind, 'text');
    expect(hello.masked, isTrue);
    expect(nodes.any((n) => n.text == 'Hello world'), isFalse);

    // TrooperUnmask shows the text.
    expect(byText('Order total')?.masked, isFalse);

    // Input: always masked, value length kept.
    final input = nodes.firstWhere((n) => n.kind == 'input');
    expect(input.text, '•••••••');
    expect(input.masked, isTrue);
    expect(nodes.any((n) => n.text?.contains('hunter') ?? false), isFalse);

    // Button with its Material color merged in.
    final button = nodes.firstWhere((n) => n.kind == 'button');
    expect(button.bg, isNotNull);
    expect(button.radius, greaterThan(0));

    final icon = nodes.firstWhere((n) => n.kind == 'icon');
    expect(icon.fg, 0xff112233);
    final toggles = nodes.where((n) => n.kind == 'switch').toList();
    expect(toggles.map((n) => n.checked), [true, false]);
    expect(toggles.first.toJson()['checked'], true);
    expect(nodes.where((n) => n.kind != 'switch').every((n) => n.checked == null), isTrue);

    final box = nodes.firstWhere((n) => n.bg == 0xffff0000);
    expect(box.kind, 'view');
    expect(box.radius, 6);
    expect(box.border, 0xff00ff00);
    expect(box.w, 40);
    expect(box.h, 20);

    final card = nodes.firstWhere((n) => n.bg == 0xff0000ff);
    expect(card.radius, greaterThan(0));

    // Opacity 0 / Offstage are skipped.
    expect(nodes.where((n) => n.text == '•••••••••'), isEmpty); // "Invisible"
    expect(nodes.where((n) => n.text == '•••••••• ••••••'), isEmpty); // "Offstage hidden"

    // AppBar and Scaffold backgrounds are there (Material → PhysicalShape).
    expect(nodes.where((n) => n.kind == 'view' && n.bg != null).length, greaterThanOrEqualTo(2));

    // List: viewport clips rows; rows outside the viewport are dropped.
    final list = nodes.firstWhere((n) => n.kind == 'list');
    expect(list.h, 50);
    final rows = nodes.where((n) => n.kind == 'text' && n.text!.startsWith('••• ')).toList();
    expect(rows.length, lessThan(10));
    expect(rows.any((n) => n.clip != null), isTrue); // partially visible row

    // Geometry: logical px within the screen, back-to-front order (scaffold bg before body text).
    final screen = tester.view.physicalSize / tester.view.devicePixelRatio;
    for (final n in nodes) {
      expect(n.x, greaterThanOrEqualTo(-0.5));
      expect(n.x + n.w, lessThanOrEqualTo(screen.width + 0.5));
    }
    final scaffoldBg = nodes.indexWhere((n) => n.kind == 'view' && n.w == screen.width && n.h == screen.height);
    expect(scaffoldBg, lessThan(nodes.indexOf(hello)));
  });

  testWidgets('maskAllText false shows text but never input values', (tester) async {
    await tester.pumpWidget(_app());
    final nodes = _capture(tester, maskAllText: false);
    expect(nodes.any((n) => n.text == 'Hello world' && !n.masked), isTrue);
    expect(nodes.firstWhere((n) => n.kind == 'input').text, '•••••••');
  });

  testWidgets('routes covered by an opaque route and dialogs', (tester) async {
    await tester.pumpWidget(_app());
    final nav = tester.state<NavigatorState>(find.byType(Navigator));
    unawaited(
      nav.push(
        MaterialPageRoute<void>(
          builder: (_) => const Scaffold(body: TrooperUnmask(child: Text('Second page'))),
        ),
      ),
    );
    await tester.pumpAndSettle();
    var nodes = _capture(tester);
    expect(nodes.any((n) => n.text == 'Second page'), isTrue);
    expect(nodes.any((n) => n.kind == 'input'), isFalse, reason: 'first page is offstage');

    unawaited(
      showDialog<void>(
        context: tester.element(find.text('Second page')),
        builder: (_) => const AlertDialog(content: TrooperUnmask(child: Text('Dialog body'))),
      ),
    );
    await tester.pumpAndSettle();
    nodes = _capture(tester);
    final page = nodes.indexWhere((n) => n.text == 'Second page');
    final dialog = nodes.indexWhere((n) => n.text == 'Dialog body');
    expect(page, isNonNegative);
    expect(dialog, greaterThan(page), reason: 'overlay draws on top');
    // Barrier (ModalBarrier → ColoredBox) between page and dialog.
    expect(nodes.sublist(page, dialog).any((n) => n.kind == 'view' && n.bg != null && n.bg! >> 24 < 0xff), isTrue);
  });

  testWidgets('node budget truncates', (tester) async {
    await tester.pumpWidget(_app());
    final size = tester.view.physicalSize / tester.view.devicePixelRatio;
    final c = ReplayMapper(maskAllText: true, maxNodes: 3).capture(tester.binding.rootElement!, size);
    expect(c.truncated, isTrue);
    expect(c.nodes, hasLength(3));
  });

  test('node JSON', () {
    final n = WireNode(
      id: 7,
      x: 1.04,
      y: 2,
      w: 10.25,
      h: 3,
      kind: 'text',
      bg: 0x80ff0000,
      fg: 0x40112233,
      radius: 4,
      text: 'a"b',
      masked: true,
      alpha: 0.5,
      clip: [1, 2, 3, 4],
    );
    expect(
      n.toString(),
      '{"id":7,"x":1,"y":2,"w":10.3,"h":3,"kind":"text","bg":"#ff000080","fg":"#112233","radius":4,'
      '"text":"a\\"b","masked":true,"alpha":0.5,"clip":[1,2,3,4]}',
    );
    expect(ReplayMasking.mask('ab c\nd'), '•• •\n•');
  });
}
