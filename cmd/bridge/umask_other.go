//go:build !unix

package main

// setUmask is a no-op where there is no umask. The container image is Linux, so
// this exists only so the bridge builds and its tests run on a Windows host.
func setUmask() {}
