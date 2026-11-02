import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

class HomePage extends StatelessWidget {
  const HomePage({super.key});

  @override
  Widget build(BuildContext context) => TextButton(
        onPressed: () => context.go('/products/42'),
        child: Text("It's ${'home'}"),
      );
}

class ProductCard extends StatelessWidget {
  const ProductCard({super.key});
  @override
  Widget build(BuildContext context) => const Text('card');
}

class ScaffoldWithNav extends StatelessWidget {
  const ScaffoldWithNav({super.key, required this.child});
  final Widget child;
  @override
  Widget build(BuildContext context) => Scaffold(body: child);
}
