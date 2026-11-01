# The plugin resolves the native TaskTrooper SDK bridge by reflection; keep it when the host app includes it.
-keep class com.tasktrooper.android.TrooperHybridBridge { public static *; }
-dontwarn com.tasktrooper.android.TrooperHybridBridge
# Native replay detection (do not double record, contracts.md §23).
-keepclassmembers class com.tasktrooper.android.replay.TrooperReplay { private static ** installed; public static boolean isRecording(); }
-dontwarn com.tasktrooper.android.replay.TrooperReplay
