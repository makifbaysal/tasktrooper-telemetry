import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';
import 'package:tasktrooper_flutter/tasktrooper_flutter.dart';

import 'screens/home_page.dart';
import 'screens/product_page.dart';

/// Doc comment: GoRoute(path: '/doc-comment')
final router = GoRouter(
  observers: [TrooperNavigatorObserver()],
  routes: [
    GoRoute(
      path: '/',
      name: 'Home',
      builder: (context, state) => const HomePage(),
      routes: [
        GoRoute(
          path: 'products/:id',
          name: 'ProductDetail',
          builder: (context, state) => ProductPage(id: state.pathParameters['id']!),
          routes: [
            GoRoute(path: 'reviews', builder: (c, s) => const ReviewsScreen()),
          ],
        ),
      ],
    ),
    ShellRoute(
      builder: (c, s, child) => ScaffoldWithNav(child: child),
      routes: [
        GoRoute(
          path: '/orders/:orderId',
          pageBuilder: (c, s) => NoTransitionPage(child: OrderDetailScreen(id: s.pathParameters['orderId']!)),
        ),
      ],
    ),
    // GoRoute(path: '/commented', builder: (c, s) => const HomePage()),
    /* GoRoute(path: '/block-commented', builder: (c, s) => const HomePage()), */
  ],
);

void main() => runApp(MaterialApp.router(routerConfig: router));
