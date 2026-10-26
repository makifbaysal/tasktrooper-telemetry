package dev.tasktrooper.reactnative

import com.facebook.react.bridge.Arguments
import com.facebook.react.bridge.ReadableMap
import com.facebook.react.bridge.ReadableType
import com.facebook.react.bridge.WritableMap

internal fun ReadableMap?.toStringMap(): Map<String, Any?> {
  if (this == null) return emptyMap()
  val out = HashMap<String, Any?>()
  val it = keySetIterator()
  while (it.hasNextKey()) {
    val k = it.nextKey()
    if (getType(k) == ReadableType.String) out[k] = getString(k)
  }
  return out
}

internal fun Map<String, String>.toWritableMap(): WritableMap {
  val out = Arguments.createMap()
  for ((k, v) in this) out.putString(k, v)
  return out
}
