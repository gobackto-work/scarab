//go:build unix

package main

import "syscall"

// setUmask makes files created by this process and its children private.
//
// The bridge spawns Pi, and Pi writes session transcripts to the workspace volume.
// Those were 0644 on a volume that is world-writable anyway (finding 7 of the first
// red-team review), so they were readable by every process in the workspace -- and
// the volume is shared with the workers. umask is inherited across exec, so setting
// it here is what makes the transcripts 0600 without patching Pi.
//
// It does not affect an explicit os.Chmod, so the bridge's own files keep the modes
// it sets deliberately.
func setUmask() {
	syscall.Umask(0o077)
}
