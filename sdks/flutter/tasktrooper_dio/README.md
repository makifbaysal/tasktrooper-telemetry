# tasktrooper_dio

Dio interceptor for [`tasktrooper_flutter`](https://pub.dev/packages/tasktrooper_flutter): network calls as RESOURCE
events + W3C `traceparent` propagation.

```yaml
dependencies:
  tasktrooper_flutter: ^0.1.0
  tasktrooper_dio: ^0.1.0
```

```dart
import 'package:tasktrooper_dio/tasktrooper_dio.dart';

final dio = Dio()..interceptors.add(TrooperDioInterceptor()); // add it first
```

Docs: [sdks/flutter/README.md](https://github.com/makifbaysal/tasktrooper-telemetry/tree/main/sdks/flutter#network).
