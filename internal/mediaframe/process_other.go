//go:build !unix

package mediaframe

import "os/exec"

// ownProcessGroup leaves cmd as it is: the service runs on Linux, where
// process_unix.go kills ffmpeg's whole group.
func ownProcessGroup(*exec.Cmd) {}
