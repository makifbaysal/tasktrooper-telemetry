import 'package:dio/dio.dart';
import 'package:http/http.dart' as http;
import 'package:sqflite/sqflite.dart';

final dio = Dio(BaseOptions(baseUrl: 'https://api.shop.example.com'));

Future<void> ping() => http.get(Uri.parse('https://status.example.io/ping'));

Future<void> search(String q) => http.get(Uri.https('search.example.net', '/v1/search', {'q': q}));

// final old = Uri.parse('https://legacy.example.com');

const doc = '''
  GoRoute(path: '/in-string', builder: (c, s) => const HomePage())
  'https://in-triple-string.example.com'
''';

Future<Database> openDb() => openDatabase('shop.db');
