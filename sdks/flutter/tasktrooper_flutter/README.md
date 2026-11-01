# tasktrooper_flutter

TaskTrooper Telemetry RUM SDK for Flutter: sessions, screen views, errors, network requests, frame vitals, app start
and wireframe session replay. Full docs: [sdks/flutter/README.md](https://github.com/makifbaysal/tasktrooper-telemetry/tree/main/sdks/flutter).

## Install

```yaml
dependencies:
  tasktrooper_flutter: ^0.1.0
  tasktrooper_dio: ^0.1.0   # only if you use Dio
```

```dart
import 'package:tasktrooper_flutter/tasktrooper_flutter.dart';
```

## Native plugin / hybrid apps

This package is a Flutter plugin (Android + iOS) implementing channel `dev.tasktrooper.telemetry/rum`
(`.ai/contracts.md` §9). Process start time for APP_START is always available on Android (API 24+) and iOS. If the
host app also includes the native SDK (`trooper-android`, or the `TaskTrooper` pod + `tasktrooper_flutter/Native`
subspec), Dart and native share one session so native crashes join it. Details:
[Native plugin / hybrid apps](https://github.com/makifbaysal/tasktrooper-telemetry/tree/main/sdks/flutter#native-plugin--hybrid-apps).

## Session replay

`TaskTrooper.init(config, appRunner: ..., replay: const TrooperReplayOptions(sampleRate: 0.1))` records a wireframe of
the Flutter UI (no pixels; text masked, inputs always masked; `TrooperUnmask(child: …)` to unmask). Details:
[Session replay](https://github.com/makifbaysal/tasktrooper-telemetry/tree/main/sdks/flutter#session-replay). Do not
enable the native SDK's replay in a Flutter app.
