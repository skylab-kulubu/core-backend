//go:build unix

package mediaframe

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own and, when its
// context ends, kills the whole group: nothing ffmpeg may start outlives it.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
