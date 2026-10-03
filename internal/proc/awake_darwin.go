//go:build darwin

package proc

import (
	"os"
	"os/exec"
	"strconv"
)

// startAwake runs `caffeinate -i -w <sy's pid>`: it prevents idle sleep
// and ends by itself if sy dies without releasing it.
func startAwake() func() {
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if cmd.Start() != nil {
		return nil
	}
	return func() {
		cmd.Process.Kill()
		cmd.Wait()
	}
}
