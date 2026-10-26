// Package frame holds the symbolicated frame model shared by all symbolicators.
package frame

// Frame is one stack frame in the /v1/symbolicate response (contracts.md §12).
type Frame struct {
	Fn           string `json:"fn"`
	File         string `json:"file"`
	Line         int    `json:"line"`
	Col          int    `json:"col"`
	InApp        bool   `json:"inApp"`
	Symbolicated bool   `json:"symbolicated"`
	// Module is the image (iOS), class (JVM) or bundle URL (JS) the frame came from.
	Module string `json:"module,omitempty"`
	// Addr is the instruction address (native frames).
	Addr string `json:"addr,omitempty"`
	// Mangled is the raw linkage name when Fn was demangled (Swift/C++).
	Mangled string `json:"mangled,omitempty"`
	// Inlined marks a frame that was inlined into the next (outer) frame.
	Inlined bool `json:"inlined,omitempty"`
	// Raw is the original line of the input stack.
	Raw string `json:"raw,omitempty"`
}
