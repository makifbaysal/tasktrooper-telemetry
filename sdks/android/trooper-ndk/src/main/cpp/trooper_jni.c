/* JNI entry points for com.tasktrooper.android.ndk.NdkCrashBridge. */
#include <jni.h>

#include "trooper_crash.h"

JNIEXPORT jboolean JNICALL
Java_com_tasktrooper_android_ndk_NdkCrashBridge_nativeInstall(JNIEnv *env, jclass cls, jstring path) {
    (void)cls;
    if (!path) return JNI_FALSE;
    const char *p = (*env)->GetStringUTFChars(env, path, NULL);
    if (!p) return JNI_FALSE;
    int rc = tt_ndk_install(p);
    (*env)->ReleaseStringUTFChars(env, path, p);
    return rc == 0 ? JNI_TRUE : JNI_FALSE;
}

JNIEXPORT void JNICALL
Java_com_tasktrooper_android_ndk_NdkCrashBridge_nativeSetContext(JNIEnv *env, jclass cls, jstring ctx) {
    (void)cls;
    if (!ctx) {
        tt_ndk_set_context("");
        return;
    }
    const char *c = (*env)->GetStringUTFChars(env, ctx, NULL);
    if (!c) return;
    tt_ndk_set_context(c);
    (*env)->ReleaseStringUTFChars(env, ctx, c);
}

JNIEXPORT jint JNICALL
Java_com_tasktrooper_android_ndk_NdkCrashBridge_nativeRefreshModules(JNIEnv *env, jclass cls) {
    (void)env;
    (void)cls;
    return tt_ndk_refresh_modules();
}
