package dev.tasktrooper.reactnative

import com.facebook.react.bridge.Promise
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.bridge.ReadableMap

/** New Architecture (TurboModule) implementation of the codegen spec `NativeTrooperRumSpec`. */
class TrooperRumModule(reactContext: ReactApplicationContext) : NativeTrooperRumSpec(reactContext) {
  override fun getName(): String = TrooperRumImpl.NAME

  override fun setContext(ctx: ReadableMap) {
    TrooperRumImpl.setContext(ctx.toStringMap())
  }

  override fun getContext(promise: Promise) {
    promise.resolve(TrooperRumImpl.getContext()?.toWritableMap())
  }

  override fun getProcessStartTime(promise: Promise) {
    promise.resolve(TrooperRumImpl.getProcessStartTime()?.toDouble())
  }

  override fun startReplay(options: ReadableMap, promise: Promise) {
    promise.resolve(startReplayFromJs(reactApplicationContext, options))
  }

  override fun stopReplay(promise: Promise) {
    promise.resolve(TrooperRumImpl.stopReplay())
  }

  override fun isReplayRecording(promise: Promise) {
    promise.resolve(TrooperRumImpl.isReplayRecording())
  }

  override fun notifyReplayError() {
    TrooperRumImpl.notifyReplayError()
  }
}
