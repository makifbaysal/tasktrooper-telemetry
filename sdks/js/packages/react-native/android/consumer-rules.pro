# TrooperRum calls the native SDK's hybrid bridge via reflection; keep it when the host app includes trooper-android.
-keep class com.tasktrooper.android.TrooperHybridBridge { public static *; }
-dontwarn com.tasktrooper.android.TrooperHybridBridge
