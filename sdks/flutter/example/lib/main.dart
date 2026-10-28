import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';
import 'package:trooper_dio/trooper_dio.dart';
import 'package:trooper_flutter/trooper_flutter.dart';

// Local stack: `docker compose -f deploy/docker-compose.yml up -d` + ingest-gw.
// Android emulator reaches the host as 10.0.2.2.
const dsn = String.fromEnvironment('TROOPER_DSN', defaultValue: 'http://tt_dev_ingest_key@localhost:4318/1');

final httpClient = TrooperHttpClient();
final dio = Dio()..interceptors.add(TrooperDioInterceptor());

final router = GoRouter(
  observers: [TrooperNavigatorObserver()],
  routes: [
    GoRoute(
      path: '/',
      name: 'Home',
      builder: (context, state) => const HomeScreen(),
      routes: [
        // No `name:` → the view name is the path template `products/:id`.
        GoRoute(
          path: 'products/:id',
          builder: (context, state) => ProductScreen(id: state.pathParameters['id']!),
        ),
      ],
    ),
  ],
);

void main() {
  TaskTrooper.init(
    const TrooperConfig(
      dsn: dsn,
      app: 'trooper-example',
      version: '1.0.0+1',
      environment: 'development',
      tracePropagationTargets: ['localhost', 'httpbin.org'],
      debug: true,
    ),
    appRunner: () => runApp(const ExampleApp()),
    // Wireframe session replay (contracts.md §23). 1.0 = every session; production default is 0.1.
    replay: const TrooperReplayOptions(sampleRate: 1.0),
  );
}

class ExampleApp extends StatelessWidget {
  const ExampleApp({super.key});

  @override
  Widget build(BuildContext context) => MaterialApp.router(title: 'TaskTrooper example', routerConfig: router);
}

class HomeScreen extends StatelessWidget {
  const HomeScreen({super.key});

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('TaskTrooper example')),
      body: ListView(
        children: [
          const Padding(
            padding: EdgeInsets.all(16),
            child: TextField(decoration: InputDecoration(labelText: 'Search (always masked in replay)')),
          ),
          const ListTile(title: TrooperUnmask(child: Text('Unmasked in replay'))),
          SwitchListTile(value: true, onChanged: (_) {}, title: const Text('A switch')),
          ListTile(
            title: const Text('Show dialog'),
            onTap: () => showDialog<void>(
              context: context,
              builder: (_) => const AlertDialog(title: Text('Hello'), content: Text('Dialogs are in the replay too.')),
            ),
          ),
          ListTile(title: const Text('Open product 42'), onTap: () => context.go('/products/42')),
          ListTile(
            title: const Text('Identify user'),
            onTap: () => TaskTrooper.identify('user-123', traits: {'plan': 'pro'}),
          ),
          ListTile(
            title: const Text('Track custom event'),
            onTap: () => TaskTrooper.track('checkout_started', props: {'items': '3'}, value: 120),
          ),
          ListTile(
            title: const Text('GET with package:http'),
            onTap: () => httpClient.get(Uri.parse('https://httpbin.org/get?q=secret')).ignore(),
          ),
          ListTile(
            title: const Text('GET with Dio'),
            onTap: () => dio.get<dynamic>('https://httpbin.org/status/500').ignore(),
          ),
          ListTile(
            title: const Text('Handled error'),
            onTap: () {
              try {
                throw const FormatException('bad input');
              } catch (e, s) {
                TaskTrooper.captureError(e, s);
              }
            },
          ),
          ListTile(
            title: const Text('Uncaught async error'),
            onTap: () => Future<void>.error(StateError('async boom')),
          ),
          ListTile(title: const Text('Flush now'), onTap: TaskTrooper.flush),
        ],
      ),
    );
  }
}

class ProductScreen extends StatelessWidget {
  const ProductScreen({super.key, required this.id});
  final String id;

  @override
  Widget build(BuildContext context) => Scaffold(
    appBar: AppBar(title: Text('Product $id')),
    body: Center(
      child: ElevatedButton(
        onPressed: () => TaskTrooper.addAction('add_to_cart', target: 'product_$id'),
        child: const Text('Add to cart'),
      ),
    ),
  );
}
