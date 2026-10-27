# TrooperRum calls the native SDK's hybrid bridge via reflection; keep it when the host app includes trooper-android.
-keep class com.tasktrooper.android.TrooperHybridBridge { public static *; }
-dontwarn com.tasktrooper.android.TrooperHybridBridge
# Session replay: TrooperReplay / ReplayOptions (trooper-replay) are also resolved via reflection when the app includes them.
-keep class com.tasktrooper.android.replay.TrooperReplay { public static *; }
-keep class com.tasktrooper.android.replay.ReplayOptions { public <init>(...); public *** get*(); }
-dontwarn com.tasktrooper.android.replay.**
