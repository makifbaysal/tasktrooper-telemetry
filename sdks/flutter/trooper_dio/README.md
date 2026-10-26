# trooper_dio

Dio interceptor for `trooper_flutter`: RESOURCE events + W3C `traceparent`. See [../README.md](../README.md#network).

```dart
final dio = Dio()..interceptors.add(TrooperDioInterceptor());
```
