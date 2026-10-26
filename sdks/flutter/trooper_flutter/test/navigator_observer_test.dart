import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

import 'helpers.dart';

void main() {
  testWidgets('named routes become views with loading time', (tester) async {
    final c = buildClient(testConfig(), FakeIngest());
    await tester.pumpWidget(
      MaterialApp(
        navigatorObservers: [TrooperNavigatorObserver(client: c)],
        routes: {'/': (_) => const Text('home'), '/products/:id': (_) => const Text('product')},
      ),
    );
    await tester.pump();
    expect(c.currentViewName, '/');

    unawaited(tester.state<NavigatorState>(find.byType(Navigator)).pushNamed('/products/:id'));
    await tester.pumpAndSettle();
    expect(c.currentViewName, '/products/:id');

    // dialogs are not views
    unawaited(showDialog<void>(context: tester.element(find.text('product')), builder: (_) => const Text('dialog')));
    await tester.pumpAndSettle();
    expect(c.currentViewName, '/products/:id');
    tester.state<NavigatorState>(find.byType(Navigator)).pop();
    await tester.pumpAndSettle();
    expect(c.currentViewName, '/products/:id');

    tester.state<NavigatorState>(find.byType(Navigator)).pop();
    await tester.pumpAndSettle();
    expect(c.currentViewName, '/');

    final events = c.pipeline.bufferedEvents
        .where((e) => e['type'] != 'SESSION_START' && e['type'] != 'VITAL')
        .toList();
    expect(events.map((e) => '${e['type']} ${e['view_name']}').toList(), [
      'VIEW_START /',
      'VIEW_END /',
      'VIEW_START /products/:id',
      'VIEW_END /products/:id',
      'VIEW_START /',
    ]);
    final starts = events.where((e) => e['type'] == 'VIEW_START').toList();
    expect((starts.first['view'] as Map)['is_initial'], true);
    expect((starts[1]['view'] as Map).containsKey('is_initial'), isFalse);
    expect(starts.map((e) => e['view_id']).toSet().length, 3); // new id per view instance
    await c.close();
  });

  testWidgets('pushReplacement and unnamed routes', (tester) async {
    final c = buildClient(testConfig(), FakeIngest());
    await tester.pumpWidget(
      MaterialApp(
        navigatorObservers: [TrooperNavigatorObserver(client: c)],
        initialRoute: '/login',
        routes: {'/login': (_) => const Text('login'), '/home': (_) => const Text('home')},
      ),
    );
    expect(c.currentViewName, '/login');
    unawaited(tester.state<NavigatorState>(find.byType(Navigator)).pushReplacementNamed('/home'));
    await tester.pumpAndSettle();
    expect(c.currentViewName, '/home');
    unawaited(
      tester
          .state<NavigatorState>(find.byType(Navigator))
          .push(MaterialPageRoute<void>(builder: (_) => const Text('x'))),
    );
    await tester.pumpAndSettle();
    expect(c.currentViewName, '/home'); // unnamed → ignored
    await c.close();
  });
}
