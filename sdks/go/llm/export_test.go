package llm

// ResetCaptureContent returns content capture to the environment default (tests only).
func ResetCaptureContent() { capture.Store(0) }
