package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// An rw killed hard (kill -9, TerminateProcess) takes its agent and what
// the agent started along: Windows' kill-on-close job, the wrapper's pipe
// elsewhere. Before the wrapper, both kept running on Linux and macOS
// until the next rw took their worktree.
func TestAgentDiesWithKilledParent(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pids := filepath.Join(t.TempDir(), "pids")
	parent := exec.Command(exe, "-test.run=^TestHelperGuardParent$")
	parent.Env = append(os.Environ(), "RW_PROC_HELPER=parent", "RW_PROC_PIDS="+pids)
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { parent.Wait(); close(exited) }()
	defer parent.Process.Kill()

	var agent, grandchild int
	for deadline := time.Now().Add(30 * time.Second); ; {
		if b, err := os.ReadFile(pids); err == nil {
			if _, err := fmt.Sscan(string(b), &agent, &grandchild); err == nil {
				break
			}
		}
		select {
		case <-exited:
			t.Fatal("the parent ended before its agent started")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent and its child did not start within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer killPid(agent)
	defer killPid(grandchild)
	if !Alive(agent) || !Alive(grandchild) {
		t.Fatalf("agent %d (alive %v) or its child %d (alive %v) is not running", agent, Alive(agent), grandchild, Alive(grandchild))
	}

	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	killed := time.Now()
	for Alive(agent) || Alive(grandchild) {
		if time.Since(killed) > 10*time.Second {
			t.Fatalf("10s after the parent was killed: agent %d alive %v, its child %d alive %v", agent, Alive(agent), grandchild, Alive(grandchild))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("agent and its child gone %s after the parent was killed", time.Since(killed).Round(time.Millisecond))
}

func killPid(pid int) {
	if p, err := os.FindProcess(pid); err == nil && Alive(pid) {
		p.Kill()
	}
}

// TestHelperGuardParent stands in for rw: it guards itself and starts an
// agent the way the runner does, then waits to be killed.
func TestHelperGuardParent(t *testing.T) {
	if os.Getenv("RW_PROC_HELPER") != "parent" {
		t.Skip("helper process")
	}
	if err := Guard(); err != nil {
		fmt.Fprintln(os.Stderr, "guard:", err)
		os.Exit(1)
	}
	exe, _ := os.Executable()
	cmd := exec.CommandContext(context.Background(), exe, "-test.run=^TestHelperGuardAgent$")
	Prepare(cmd)
	cmd.Env = append(os.Environ(), "RW_PROC_HELPER=agent")
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start agent:", err)
		os.Exit(1)
	}
	Started(cmd)
	time.Sleep(5 * time.Minute)
	os.Exit(1)
}

// TestHelperGuardAgent stands in for an agent CLI: it starts a child that
// stays in its process group (and job), writes both pids, and sleeps.
func TestHelperGuardAgent(t *testing.T) {
	if os.Getenv("RW_PROC_HELPER") != "agent" {
		t.Skip("helper process")
	}
	exe, _ := os.Executable()
	child := exec.Command(exe, "-test.run=^TestHelperGuardSleep$")
	child.Env = append(os.Environ(), "RW_PROC_HELPER=sleep")
	if err := child.Start(); err != nil {
		os.Exit(1)
	}
	file := os.Getenv("RW_PROC_PIDS")
	line := strconv.Itoa(os.Getpid()) + " " + strconv.Itoa(child.Process.Pid) + "\n"
	if os.WriteFile(file+".tmp", []byte(line), 0o644) != nil || os.Rename(file+".tmp", file) != nil {
		os.Exit(1)
	}
	time.Sleep(5 * time.Minute)
	os.Exit(1)
}

// TestHelperGuardSleep is the agent's child.
func TestHelperGuardSleep(t *testing.T) {
	if os.Getenv("RW_PROC_HELPER") != "sleep" {
		t.Skip("helper process")
	}
	time.Sleep(5 * time.Minute)
	os.Exit(1)
}
