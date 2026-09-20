package claudeguards

// Segments is the exported face of segments(): the commands a shell string
// runs, quote-aware, runner payloads unwrapped. Other applets (memo's
// PreToolUse hook) use it so the ONE definition of shell segmentation stays
// here.
func Segments(cmd string) []string { return segments(cmd) }

// ShellFields splits one segment into argv-shaped fields with quotes removed.
func ShellFields(segment string) []string { return shellFields(segment) }
