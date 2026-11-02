import 'package:flutter/material.dart';

class ProductPage extends StatelessWidget {
  const ProductPage({super.key, required this.id});
  final String id;
  @override
  Widget build(BuildContext context) => Text(r'raw $id \n');
}

class ReviewsScreen extends StatelessWidget {
  const ReviewsScreen({super.key});
  @override
  Widget build(BuildContext context) => const Text('reviews');
}

class OrderDetailScreen extends StatelessWidget {
  const OrderDetailScreen({super.key, required this.id});
  final String id;
  @override
  Widget build(BuildContext context) => Text('order $id');
}

class SettingsScreen extends StatefulWidget {
  const SettingsScreen({super.key});
  @override
  State<SettingsScreen> createState() => _SettingsScreenState();
}

class _SettingsScreenState extends State<SettingsScreen> {
  @override
  Widget build(BuildContext context) => const Text('settings');
}
