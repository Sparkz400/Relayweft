package morning

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func at(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, time.Local) }

func TestBuild(t *testing.T) {
	since, until := at(6, 18, 0), at(7, 7, 30)
	states := []orchestrator.TaskState{
		{ID: "a", Task: "fix the flaky parser test", Dir: "D:/repo", Status: "done", Created: at(6, 23, 0), Updated: at(6, 23, 20), UndoKey: "k-a", Unattended: true},
		{ID: "b", Task: "add --json to rw history\n\nFollow-up: and tests", Dir: "D:/repo", Status: "failed", Created: at(7, 1, 0), Updated: at(7, 1, 30), Summary: "checks failed: go test", Unattended: true},
		{ID: "c", Task: "refactor the budget check", Dir: "D:/other", Status: "running", Created: at(7, 2, 0), Updated: at(7, 2, 40), Unattended: true,
			Saved: []orchestrator.SavedEdits{{Step: "w", Branch: "rw/saved/c-w"}}},
		{ID: "d", Task: "merge both", Dir: "D:/repo", Status: "done", Created: at(7, 3, 0), Updated: at(7, 3, 10), Unattended: true, Kept: []string{"rw/kept/d-x"}},
		// Watched, not unattended: only with All.
		{ID: "e", Task: "typed by hand", Dir: "D:/repo", Status: "done", Created: at(6, 19, 0), Updated: at(6, 19, 5)},
		// Outside the window.
		{ID: "f", Task: "yesterday", Dir: "D:/repo", Status: "done", Created: at(6, 9, 0), Updated: at(6, 9, 30), Unattended: true},
		{ID: "g", Task: "later", Dir: "D:/repo", Status: "done", Created: at(7, 8, 0), Updated: at(7, 8, 30), Unattended: true},
	}
	until1 := at(7, 3, 0)
	recs := []sessionlog.Record{
		{Type: sessionlog.TypeTaskEnd, TaskID: "a", Cost: &event.TaskCost{PerProvider: map[string]event.TokenUsage{"claude": {Input: 100_000}}, CostUSD: 1.5}},
		{Type: sessionlog.TypeTaskEnd, TaskID: "b", Cost: &event.TaskCost{PerProvider: map[string]event.TokenUsage{"codex": {Input: 50_000}, "claude": {Output: 20_000}}}},
		{Type: sessionlog.TypeTaskEnd, TaskID: "e", Cost: &event.TaskCost{PerProvider: map[string]event.TokenUsage{"claude": {Input: 999_999}}}},
		{Type: sessionlog.TypeLimit, TS: at(7, 1, 20), Provider: "claude", Until: &until1, Unavailable: sessionlog.Bool(false)},
		{Type: sessionlog.TypeLimit, TS: at(7, 1, 21), Provider: "claude", Until: &until1, Unavailable: sessionlog.Bool(false)}, // same limit
		{Type: sessionlog.TypeLimit, TS: at(7, 1, 22), Provider: "gemini", Until: &until1, Unavailable: sessionlog.Bool(true)},
	}
	interrupted := func(s orchestrator.TaskState) bool { return s.ID == "c" }
	s := Build(states, recs, Options{Since: since, Until: until, Interrupted: interrupted})
	var ids []string
	for _, t := range s.Tasks {
		ids = append(ids, t.ID)
	}
	if strings.Join(ids, ",") != "a,b,c,d" {
		t.Fatalf("tasks = %v", ids)
	}
	if s.Done != 2 || s.Failed != 1 || s.Interrupted != 1 || s.Running != 0 {
		t.Fatalf("counts = %+v", s)
	}
	if s.Tokens["claude"] != 120_000 || s.Tokens["codex"] != 50_000 || s.USD != 1.5 {
		t.Fatalf("usage = %v $%v", s.Tokens, s.USD)
	}
	if len(s.Limits) != 1 || s.Limits[0].Provider != "claude" {
		t.Fatalf("limits = %+v", s.Limits)
	}
	if s.Tasks[1].Text != "add --json to rw history" || s.Tasks[2].Project != "other" || s.Tasks[2].Ended.IsZero() == false {
		t.Fatalf("tasks = %+v", s.Tasks)
	}
	var cmds []string
	for _, a := range s.Actions {
		cmds = append(cmds, a.Command)
	}
	if got := strings.Join(cmds, "|"); got != "rw report b|rw resume c|git diff HEAD...rw/kept/d-x" {
		t.Fatalf("actions = %s", got)
	}
	if got := s.Title(); got != "2 of 4 task(s) done, 1 failed, 1 interrupted - 3 thing(s) need you" {
		t.Fatalf("title = %q", got)
	}
	text := s.Text()
	for _, w := range []string{"Since Tue 18:00: 2 of 4", "Needs you:", "  - failed: add --json to rw history  →  rw report b",
		"Tue 23:00  done        repo: fix the flaky parser test · 20m0s · undo: rw undo k-a", "      checks failed: go test", "  01:00      failed      repo: add --json",
		"Used: codex 50k · claude 120k fresh tokens · ≈$1.50 API-equivalent", "Limits hit: claude at 01:20 (until 03:00)"} {
		if !strings.Contains(text, w) {
			t.Errorf("text lacks %q:\n%s", w, text)
		}
	}
	// With All, the watched task too; with Dir, one project.
	if s := Build(states, recs, Options{Since: since, Until: until, All: true, Interrupted: interrupted}); len(s.Tasks) != 5 {
		t.Fatalf("all: %d tasks", len(s.Tasks))
	}
	if s := Build(states, recs, Options{Since: since, Until: until, Dir: `d:\other\`, Interrupted: interrupted}); len(s.Tasks) != 1 || s.Tasks[0].ID != "c" {
		t.Fatalf("dir: %+v", s.Tasks)
	}
	empty := Build(nil, nil, Options{Since: since, Until: until})
	if !empty.Empty() || !strings.Contains(empty.Text(), "nothing ran unattended since Tue 18:00") || empty.Tasks == nil {
		t.Fatalf("empty = %q %+v", empty.Text(), empty)
	}
}

func TestParseSince(t *testing.T) {
	now := at(7, 7, 30)
	for in, want := range map[string]time.Time{
		"12h": at(6, 19, 30), "2d": at(5, 7, 30), "90m": at(7, 6, 0),
		"18:00": at(6, 18, 0), "06:00": at(7, 6, 0), "6pm": at(6, 18, 0),
	} {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%s: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "yesterday", "-2h", "0d"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestDailySendsOncePerDay(t *testing.T) {
	cfg := config.Default()
	cfg.LogDir = t.TempDir()
	cfg.Notify.Morning = "07:30"
	var mu sync.Mutex
	clock := at(7, 7, 28)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(time.Minute)
		return clock
	}
	var sent []Options
	ctx, cancel := context.WithCancel(context.Background())
	collect := func(_ *config.Config, o Options) Summary {
		return Summary{Since: o.Since, Until: o.Until, Tasks: []Task{{ID: "a"}}}
	}
	deliver := func(_ context.Context, _ *config.Config, s Summary, source string) error {
		sent = append(sent, Options{Since: s.Since, Until: s.Until})
		if source != "pc" {
			t.Errorf("source = %q", source)
		}
		cancel()
		return nil
	}
	daily(ctx, func() *config.Config { return cfg }, "pc", nil, now, collect, deliver, time.Millisecond)
	if len(sent) != 1 || !sent[0].Until.Equal(at(7, 7, 30)) || !sent[0].Since.Equal(at(6, 7, 30)) {
		t.Fatalf("sent = %+v", sent)
	}
	// Another rw at the same time: the day's marker is taken.
	clock = at(7, 7, 28)
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	daily(ctx, func() *config.Config { return cfg }, "pc", nil, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if clock.Before(at(7, 7, 35)) {
			clock = clock.Add(time.Minute)
		}
		return clock
	}, collect, deliver, time.Millisecond)
	if len(sent) != 1 {
		t.Fatalf("sent twice: %+v", sent)
	}
	// An empty night sends nothing.
	cfg.LogDir = t.TempDir()
	clock = at(7, 7, 28)
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	daily(ctx, func() *config.Config { return cfg }, "pc", nil, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if clock.Before(at(7, 7, 35)) {
			clock = clock.Add(time.Minute)
		}
		return clock
	}, func(*config.Config, Options) Summary { return Summary{} }, deliver, time.Millisecond)
	if len(sent) != 1 {
		t.Fatalf("an empty night was sent: %+v", sent)
	}
	if _, _, err := ParseAt("7.30"); err == nil {
		t.Error("ParseAt accepted 7.30")
	}
}
