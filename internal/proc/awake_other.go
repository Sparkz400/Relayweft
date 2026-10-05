//go:build !windows && !darwin

package proc

// startAwake does nothing here: Linux desktops differ too much (systemd
// inhibit, DE settings) to keep a machine awake reliably from rw.
func startAwake() func() { return nil }
