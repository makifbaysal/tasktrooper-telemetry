# Host (macOS/Linux) harness for the async-signal-safe handler + unwinder: forks children that crash
# for real and checks the report each one leaves behind. Run via `./gradlew :trooper-ndk:hostNativeTest`.
cmake_minimum_required(VERSION 3.18)
project(trooper_ndk_host_test C)
set(CMAKE_C_STANDARD 11)
add_executable(crash_harness crash_harness.c ../../main/cpp/trooper_crash.c)
target_include_directories(crash_harness PRIVATE ../../main/cpp)
target_compile_options(crash_harness PRIVATE -Wall -Wextra -Werror -O0 -g -fno-omit-frame-pointer)
if(NOT APPLE)
  target_link_options(crash_harness PRIVATE -Wl,--build-id=sha1 -rdynamic)
  target_link_libraries(crash_harness PRIVATE dl pthread)
endif()
enable_testing()
add_test(NAME crash_harness COMMAND crash_harness ${CMAKE_CURRENT_BINARY_DIR}/reports)
