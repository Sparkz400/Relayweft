package proc

// syscallKillGroup: the tests that start process groups do not run on
// Windows.
func syscallKillGroup(pid int) {}
