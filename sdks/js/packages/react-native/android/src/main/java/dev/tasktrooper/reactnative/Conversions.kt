package dev.tasktrooper.reactnative

import com.facebook.react.bridge.Arguments
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.bridge.ReadableMap
import com.facebook.react.bridge.ReadableType
import com.facebook.react.bridge.WritableMap

/** JS replay options → native replay install (current Activity first, so replay sees the window that is already resumed). */
internal fun startReplayFromJs(context: ReactApplicationContext, options: ReadableMap?): Boolean =
  TrooperRumImpl.startReplay(
    context.currentActivity ?: context,
    options.doubleOrNull("sampleRate"),
    options.doubleOrNull("onErrorSampleRate"),
    options.booleanOrNull("maskAllText"),
  )

/** String values plus booleans (`sampled`, contracts.md §9); other types are dropped. */
internal fun ReadableMap?.toStringMap(): Map<String, Any?> {
  if (this == null) return emptyMap()
  val out = HashMap<String, Any?>()
  val it = keySetIterator()
  while (it.hasNextKey()) {
    val k = it.nextKey()
    when (getType(k)) {
      ReadableType.String -> out[k] = getString(k)
      ReadableType.Boolean -> out[k] = getBoolean(k)
      else -> {}
    }
  }
  return out
}

internal fun ReadableMap?.doubleOrNull(key: String): Double? =
  if (this != null && hasKey(key) && getType(key) == ReadableType.Number) getDouble(key) else null

internal fun ReadableMap?.booleanOrNull(key: String): Boolean? =
  if (this != null && hasKey(key) && getType(key) == ReadableType.Boolean) getBoolean(key) else null

internal fun Map<String, String>.toWritableMap(): WritableMap {
  val out = Arguments.createMap()
  for ((k, v) in this) out.putString(k, v)
  return out
}
