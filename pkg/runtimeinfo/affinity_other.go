//go:build !linux

package runtimeinfo

// readAffinity is a stub on non-Linux platforms. The harness only
// targets RHEL in production; macOS and other developer hosts return
// the empty string and the JSONL event omits the field.
func readAffinity() string {
	return ""
}
