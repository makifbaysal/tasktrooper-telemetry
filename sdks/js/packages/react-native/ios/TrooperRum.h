#import <Foundation/Foundation.h>

#ifdef RCT_NEW_ARCH_ENABLED
#import <TrooperRumSpec/TrooperRumSpec.h>

@interface TrooperRum : NSObject <NativeTrooperRumSpec>
#else
#import <React/RCTBridgeModule.h>

@interface TrooperRum : NSObject <RCTBridgeModule>
#endif

@end
