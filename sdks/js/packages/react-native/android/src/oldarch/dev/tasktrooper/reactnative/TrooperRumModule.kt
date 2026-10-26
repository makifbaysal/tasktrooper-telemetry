package dev.tasktrooper.reactnative

import com.facebook.react.bridge.Promise
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.bridge.ReactContextBaseJavaModule
import com.facebook.react.bridge.ReactMethod
import com.facebook.react.bridge.ReadableMap

/** Legacy bridge (Old Architecture) implementation; same API as the TurboModule spec. */
class TrooperRumModule(reactContext: ReactApplicationContext) : ReactContextBaseJavaModule(reactContext) {
  override fun getName(): String = TrooperRumImpl.NAME

  @ReactMethod
  fun setContext(ctx: ReadableMap?) {
    TrooperRumImpl.setContext(ctx.toStringMap())
  }

  @ReactMethod
  fun getContext(promise: Promise) {
    promise.resolve(TrooperRumImpl.getContext()?.toWritableMap())
  }

  @ReactMethod
  fun getProcessStartTime(promise: Promise) {
    promise.resolve(TrooperRumImpl.getProcessStartTime()?.toDouble())
  }
}
