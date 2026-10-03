package main

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/diag"
	"github.com/sparkz400/switchyard/internal/orchestrator"
	"github.com/sparkz400/switchyard/internal/proc"
	"github.com/sparkz400/switchyard/internal/sysload"
	"gopkg.in/yaml.v3"
)

// startDiag opens the debug log and records how sy was started.
func startDiag(sub string) {
	diag.Version = version
	if err := diag.Init(diag.DefaultDir()); err != nil {
		fmt.Fprintln(os.Stderr, "warning: debug log disabled:", err)
	}
	wd, _ := os.Getwd()
	diag.Logf("=== sy %s %s (%s/%s, %d cpus) cwd=%s args=%q", version, sub, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), wd, os.Args[1:])
	diag.Logf("terminal: WT_SESSION=%t TERM=%q TERM_PROGRAM=%q", os.Getenv("WT_SESSION") != "", os.Getenv("TERM"), os.Getenv("TERM_PROGRAM"))
	for _, e := range badPath {
		diag.Logf("PATH entry with stray quote repaired: %s", e)
	}
}

// crashGuard is deferred in main: a panic is written to a crash log and the
// user is told where it is and how to report it.
func crashGuard() {
	if r := recover(); r != nil {
		path := diag.Crash("main", r, debug.Stack())
		fmt.Fprintf(os.Stderr, "\nsy crashed: %v\ncrash log: %s\nplease run `sy bugreport` and send the zip it creates\n", r, path)
		os.Exit(3)
	}
}

// doctorMachine reports load protection, free disk and the worktree pools.
func doctorMachine(w io.Writer, cfg *config.Config, ok func(bool) string, warn string) (problems int) {
	oc := cfg.Orchestrator
	s := sysload.NewSampler(500 * time.Millisecond)
	s.Get()
	time.Sleep(600 * time.Millisecond)
	sm := s.Get()
	load := "unknown on this OS"
	if sm.CPUOK {
		load = fmt.Sprintf("CPU %.0f%%", sm.CPU*100)
	}
	if sm.MemOK {
		load += fmt.Sprintf(", %s of %s RAM free", orchestrator.HumanBytes(sm.MemFree), orchestrator.HumanBytes(sm.MemTotal))
	}
	prot := []string{}
	if oc.LowPriority {
		prot = append(prot, "agents at low priority")
	}
	if oc.MaxCPUPercent > 0 {
		prot = append(prot, fmt.Sprintf("hold new agents above %d%% CPU", oc.MaxCPUPercent))
	}
	if oc.MinFreeMemoryMB > 0 {
		prot = append(prot, fmt.Sprintf("or below %d MB free RAM", oc.MinFreeMemoryMB))
	}
	if len(prot) == 0 {
		prot = append(prot, "off")
	}
	fmt.Fprintf(w, "%s machine     %s; protection: %s\n", ok(true), load, strings.Join(prot, ", "))

	cache, _ := os.UserCacheDir()
	if free, fine := sysload.DiskFree(cache); fine {
		min := uint64(oc.MinFreeDiskGB * (1 << 30))
		mark := ok(true)
		note := ""
		if min > 0 && free < min {
			mark = warn
			note = fmt.Sprintf(" - below min_free_disk_gb %.0f: no new pool worktrees", oc.MinFreeDiskGB)
		}
		fmt.Fprintf(w, "%s disk        %s free for pool worktrees (%s)%s\n", mark, orchestrator.HumanBytes(free), cache, note)
	}
	pools := orchestrator.Pools()
	sort.Slice(pools, func(i, j int) bool { return pools[i].Bytes > pools[j].Bytes })
	var total uint64
	for _, p := range pools {
		total += p.Bytes
		mark := ok(true)
		note := ""
		if warnAt := uint64(oc.PoolWarnGB * (1 << 30)); warnAt > 0 && p.Bytes > warnAt {
			mark = warn
			note = " - large: `sy clean` in that repo frees it"
		}
		idle := "never used"
		if !p.LastUsed.IsZero() {
			idle = "used " + ago(p.LastUsed)
		}
		repo := p.Repo
		if repo == "" {
			repo = p.Dir
		}
		fmt.Fprintf(w, "%s pool        %s: %d slot(s), %s, %s%s\n", mark, repo, p.Slots, orchestrator.HumanBytes(p.Bytes), idle, note)
	}
	if len(pools) == 0 {
		fmt.Fprintf(w, "%s pool        no worktree pools yet\n", ok(true))
	} else if len(pools) > 1 {
		fmt.Fprintf(w, "%s pool        %s in total; slots idle longer than %s are pruned automatically\n", ok(true), orchestrator.HumanBytes(total), oc.PoolMaxIdle.D())
	}
	fmt.Fprintf(w, "%s debug log   %s\n", ok(true), filepath.Join(diag.Dir(), "sy-debug.log"))
	return 0
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// prunePoolsInBackground removes idle pool worktrees without delaying startup.
func prunePoolsInBackground(cfg *config.Config) {
	maxIdle := cfg.Orchestrator.PoolMaxIdle.D()
	if maxIdle <= 0 {
		return
	}
	go func() {
		defer diag.Recover("pool prune", nil)
		if n, freed := orchestrator.PrunePools(maxIdle); n > 0 {
			diag.Logf("pruned %d pool worktree(s) idle > %s, freed %s", n, maxIdle, orchestrator.HumanBytes(freed))
		}
	}()
}

// cmdBugreport bundles everything needed to diagnose a problem into one zip.
func cmdBugreport(args []string) error {
	fs := flag.NewFlagSet("sy bugreport", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file")
	out := fs.String("out", "", "zip file to write (default ./sy-bugreport-<time>.zip)")
	sessions := fs.Int("sessions", 3, "number of recent session logs to include")
	fs.Parse(args)
	if *out == "" {
		*out = "sy-bugreport-" + time.Now().Format("20060102-150405") + ".zip"
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	add := func(name string, data []byte) {
		if w, err := zw.Create(name); err == nil {
			w.Write(data)
		}
	}
	addFile := func(name, path string) {
		if data, err := os.ReadFile(path); err == nil {
			add(name, data)
		}
	}

	// Environment.
	var env bytes.Buffer
	fmt.Fprintf(&env, "switchyard %s\nos %s/%s, go %s, %d cpus\ntime %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(), time.Now().Format(time.RFC3339))
	wd, _ := os.Getwd()
	fmt.Fprintf(&env, "cwd %s\n", wd)
	for _, k := range []string{"WT_SESSION", "TERM", "TERM_PROGRAM", "COLORTERM", "ComSpec", "PATHEXT"} {
		fmt.Fprintf(&env, "%s=%s\n", k, os.Getenv(k))
	}
	fmt.Fprintf(&env, "\nPATH entries:\n")
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		fmt.Fprintf(&env, "  %s\n", p)
	}
	for _, e := range badPath {
		fmt.Fprintf(&env, "repaired PATH entry with stray quote: %s\n", e)
	}
	for _, c := range []string{"git", "codex", "claude"} {
		if bin, err := proc.Resolve(c); err == nil {
			v, _ := exec.Command(bin, "--version").CombinedOutput()
			fmt.Fprintf(&env, "%s: %s (%s)\n", c, strings.TrimSpace(string(v)), bin)
		} else {
			fmt.Fprintf(&env, "%s: not found\n", c)
		}
	}
	add("environment.txt", env.Bytes())

	// Doctor, config.
	var doc bytes.Buffer
	if err := runDoctor(&doc, *cfgPath); err != nil {
		fmt.Fprintf(&doc, "\ndoctor: %v\n", err)
	}
	add("doctor.txt", []byte(ansi.Strip(doc.String())))
	if cfg, path, err := config.Load(*cfgPath); err == nil {
		cfg.MCP = cfg.MCP.Redacted() // env and header values may be secrets
		data, _ := yaml.Marshal(cfg)
		add("config.yaml", append([]byte("# effective config, loaded from "+path+"\n"), data...))

		// Recent session logs.
		files, _ := filepath.Glob(filepath.Join(cfg.SessionDir(), "*.jsonl"))
		sort.Strings(files)
		if len(files) > *sessions {
			files = files[len(files)-*sessions:]
		}
		for _, p := range files {
			addFile("sessions/"+filepath.Base(p), p)
		}
	}

	// Debug and crash logs.
	logDir := diag.DefaultDir()
	diag.Logf("bugreport written to %s", *out)
	addFile("logs/sy-debug.log", filepath.Join(logDir, "sy-debug.log"))
	addFile("logs/sy-debug.log.1", filepath.Join(logDir, "sy-debug.log.1"))
	crashes, _ := filepath.Glob(filepath.Join(logDir, "crash-*.log"))
	sort.Strings(crashes)
	if len(crashes) > 10 {
		crashes = crashes[len(crashes)-10:]
	}
	for _, p := range crashes {
		addFile("logs/"+filepath.Base(p), p)
	}
	if err := zw.Close(); err != nil {
		return err
	}
	abs, _ := filepath.Abs(*out)
	fmt.Printf("wrote %s\n", abs)
	fmt.Println("It contains: environment and PATH, sy doctor output, your effective config, the last",
		*sessions, "session logs\n(task texts and agent output), the debug log and crash logs. No API keys or tokens are read.")
	return nil
}
