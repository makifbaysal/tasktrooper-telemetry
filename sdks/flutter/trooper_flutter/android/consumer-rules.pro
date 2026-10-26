# The plugin resolves the native TaskTrooper SDK bridge by reflection; keep it when the host app includes it.
-keep class com.tasktrooper.android.TrooperHybridBridge { public static *; }
-dontwarn com.tasktrooper.android.TrooperHybridBridge
