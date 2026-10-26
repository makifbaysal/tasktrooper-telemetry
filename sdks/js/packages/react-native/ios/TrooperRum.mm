// TrooperRum native module: contracts.md §9 hybrid bridge (channel `dev.tasktrooper.telemetry/rum`).
#import "TrooperRum.h"

#if __has_include("TrooperReactNative-Swift.h")
#import "TrooperReactNative-Swift.h"
#else
#import <TrooperReactNative/TrooperReactNative-Swift.h>
#endif

@implementation TrooperRum

RCT_EXPORT_MODULE(TrooperRum)

+ (BOOL)requiresMainQueueSetup
{
  return NO;
}

#ifdef RCT_NEW_ARCH_ENABLED

- (void)setContext:(NSDictionary *)ctx
{
  [TrooperRumBridge setContext:ctx];
}

- (void)getContext:(RCTPromiseResolveBlock)resolve reject:(RCTPromiseRejectBlock)reject
{
  resolve([TrooperRumBridge getContext]);
}

- (void)getProcessStartTime:(RCTPromiseResolveBlock)resolve reject:(RCTPromiseRejectBlock)reject
{
  resolve([TrooperRumBridge processStartTime]);
}

- (std::shared_ptr<facebook::react::TurboModule>)getTurboModule:
    (const facebook::react::ObjCTurboModule::InitParams &)params
{
  return std::make_shared<facebook::react::NativeTrooperRumSpecJSI>(params);
}

#else

RCT_EXPORT_METHOD(setContext:(NSDictionary *)ctx)
{
  [TrooperRumBridge setContext:ctx];
}

RCT_EXPORT_METHOD(getContext:(RCTPromiseResolveBlock)resolve reject:(RCTPromiseRejectBlock)reject)
{
  resolve([TrooperRumBridge getContext]);
}

RCT_EXPORT_METHOD(getProcessStartTime:(RCTPromiseResolveBlock)resolve reject:(RCTPromiseRejectBlock)reject)
{
  resolve([TrooperRumBridge processStartTime]);
}

#endif

@end
