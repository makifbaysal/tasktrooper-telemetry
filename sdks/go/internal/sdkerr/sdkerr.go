// Package sdkerr lets sub-packages of the trooper SDK (llm) count swallowed errors in the
// trooper.sdk.errors self-metric without importing the root package.
package sdkerr

import "sync/atomic"

var counter atomic.Pointer[func(component string)]

// SetCounter installs the error counter (called by the root package).
func SetCounter(f func(component string)) { counter.Store(&f) }

// Count records a swallowed error for component. It is a no-op until SetCounter was called.
func Count(component string) {
	if f := counter.Load(); f != nil && *f != nil {
		(*f)(component)
	}
}
