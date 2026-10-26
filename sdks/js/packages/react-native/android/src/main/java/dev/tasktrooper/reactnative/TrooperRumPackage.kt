package dev.tasktrooper.reactnative

import com.facebook.react.BaseReactPackage
import com.facebook.react.bridge.NativeModule
import com.facebook.react.bridge.ReactApplicationContext
import com.facebook.react.module.model.ReactModuleInfo
import com.facebook.react.module.model.ReactModuleInfoProvider

class TrooperRumPackage : BaseReactPackage() {
  override fun getModule(name: String, reactContext: ReactApplicationContext): NativeModule? =
    if (name == TrooperRumImpl.NAME) TrooperRumModule(reactContext) else null

  override fun getReactModuleInfoProvider(): ReactModuleInfoProvider = ReactModuleInfoProvider {
    mapOf(
      TrooperRumImpl.NAME to ReactModuleInfo(
        TrooperRumImpl.NAME,
        TrooperRumModule::class.java.name,
        false, // canOverrideExistingModule
        false, // needsEagerInit
        false, // isCxxModule
        BuildConfig.IS_NEW_ARCHITECTURE_ENABLED, // isTurboModule
      )
    )
  }
}
