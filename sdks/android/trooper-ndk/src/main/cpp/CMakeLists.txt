cmake_minimum_required(VERSION 3.22.1)
project(trooper_ndk C)

add_library(trooper-ndk SHARED trooper_crash.c trooper_jni.c)
# Frame pointers make the fallback unwinder useful for our own frames; build-id for server symbolication.
target_compile_options(trooper-ndk PRIVATE -Wall -Wextra -Werror -fno-omit-frame-pointer -std=c11)
target_link_options(trooper-ndk PRIVATE -Wl,--build-id=sha1 -Wl,-z,max-page-size=16384)
