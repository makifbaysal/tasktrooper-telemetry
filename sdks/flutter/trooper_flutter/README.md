# trooper_flutter

TaskTrooper Telemetry RUM SDK for Flutter. See [../README.md](../README.md).

## Native plugin / hybrid apps

This package is a Flutter plugin (Android + iOS) implementing channel `dev.tasktrooper.telemetry/rum`
(`.ai/contracts.md` §9). Process start time for APP_START is always available on Android (API 24+) and iOS. If the
host app also includes the native SDK (`trooper-android`, or the `TaskTrooper` pod + `trooper_flutter/Native`
subspec), Dart and native share one session so native crashes join it. Details: [../README.md](../README.md#native-plugin--hybrid-apps).
