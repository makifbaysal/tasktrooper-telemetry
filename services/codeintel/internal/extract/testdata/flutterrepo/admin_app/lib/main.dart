import 'package:flutter/material.dart';
import 'package:cloud_firestore/cloud_firestore.dart';
import 'package:tasktrooper_flutter/tasktrooper_flutter.dart';

void main() => runApp(const AdminApp());

class AdminApp extends StatelessWidget {
  const AdminApp({super.key});

  @override
  Widget build(BuildContext context) => MaterialApp(
        navigatorObservers: [TrooperNavigatorObserver()],
        routes: {
          '/': (context) => const DashboardPage(),
          UsersPage.routeName: (context) => const UsersPage(),
          '/reports': (context) => ReportsScreen(),
        },
        onGenerateRoute: (settings) {
          switch (settings.name) {
            case '/users/edit':
              return MaterialPageRoute(builder: (_) => const EditUserPage(), settings: settings);
          }
          return null;
        },
      );
}

class DashboardPage extends StatelessWidget {
  const DashboardPage({super.key});
  @override
  Widget build(BuildContext context) => const Text('dashboard');
}

class UsersPage extends StatelessWidget {
  const UsersPage({super.key});
  static const routeName = '/users';

  @override
  Widget build(BuildContext context) => TextButton(
        onPressed: () => Navigator.pushNamed(context, '/reports'),
        child: StreamBuilder(stream: FirebaseFirestore.instance.collection('users').snapshots(), builder: (c, s) => const Text('u')),
      );
}

class ReportsScreen extends StatelessWidget {
  @override
  Widget build(BuildContext context) => const Text('reports');
}

class EditUserPage extends StatelessWidget {
  const EditUserPage({super.key});
  @override
  Widget build(BuildContext context) => const Text('edit');
}

class AuditLogPage extends StatelessWidget {
  const AuditLogPage({super.key});
  @override
  Widget build(BuildContext context) => const Text('audit');
}
