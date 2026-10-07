package dayplan

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/limits"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

func quotaRec(ts time.Time, p string, q event.QuotaInfo) sessionlog.Record {
	return sessionlog.Record{Type: sessionlog.TypeQuota, TS: ts, Provider: p, Quota: &q}
}

func agentEnd(ts time.Time, p string, tokens int64) sessionlog.Record {
	return sessionlog.Record{Type: sessionlog.TypeAgentEnd, TS: ts, Provider: p, Tokens: &event.TokenUsage{Input: tokens}}
}

func taskEnd(ts time.Time, cwd string, tokens int64, d time.Duration) sessionlog.Record {
	return sessionlog.Record{Type: sessionlog.TypeTaskEnd, TS: ts, Cwd: cwd, Mode: "routed", DurationMS: d.Milliseconds(),
		Cost: &event.TaskCost{PerProvider: map[string]event.TokenUsage{"claude": {Input: tokens}}, CostUSD: 0.5}}
}

func TestLearnCapacityAndTypicalTask(t *testing.T) {
	t0 := at(20, 0)
	short := func(u float64, resets time.Time) event.QuotaInfo {
		return event.QuotaInfo{Utilization: u, Window: "five_hour", ResetsAt: resets, Windows: map[string]float64{"five_hour": u, "seven_day": 0.1}}
	}
	recs := []sessionlog.Record{
		// Out of order on purpose: several session files are read.
		agentEnd(t0.Add(20*time.Minute), "claude", 300_000),
		quotaRec(t0, "claude", short(0.2, t0.Add(3*time.Hour))),
		quotaRec(t0.Add(30*time.Minute), "claude", short(0.3, t0.Add(3*time.Hour))),
		agentEnd(t0.Add(40*time.Minute), "claude", 100_000),
		agentEnd(t0.Add(41*time.Minute), "codex", 999_999), // another provider's tokens
		quotaRec(t0.Add(50*time.Minute), "claude", short(0.35, t0.Add(3*time.Hour))),
		// A drop (a new window) is no sample.
		agentEnd(t0.Add(60*time.Minute), "claude", 50_000),
		quotaRec(t0.Add(4*time.Hour), "claude", short(0.05, t0.Add(8*time.Hour))),

		taskEnd(t0, "D:/repo", 100_000, 4*time.Minute),
		taskEnd(t0, `d:\repo\`, 200_000, 6*time.Minute),
		taskEnd(t0, "D:/repo", 300_000, 8*time.Minute),
		taskEnd(t0, "D:/other", 5_000_000, time.Hour),
		{Type: sessionlog.TypeTaskEnd, TS: t0, Cwd: "D:/repo", Mode: "demo", DurationMS: 1, Cost: &event.TaskCost{}},
	}
	h := Learn(recs, "D:/repo")
	c, n := h.Capacity("claude")
	// 300k over 10 points = 3M; 100k over 5 points = 2M.
	if n != 2 || math.Abs(c-2_500_000) > 1 {
		t.Fatalf("capacity = %v from %d samples, want 2.5M from 2", c, n)
	}
	if c, n := h.Capacity("codex"); c != 0 || n != 0 {
		t.Fatalf("codex capacity = %v (%d)", c, n)
	}
	if !h.Repo || h.Tasks != 3 || h.TaskTokens != 200_000 || h.TaskDuration != 6*time.Minute || h.TaskUSD != 0.5 {
		t.Fatalf("typical task = %+v", h)
	}
	// Elsewhere: too few own tasks, so every repo's count.
	h2 := Learn(recs, "D:/new")
	if h2.Repo || h2.Tasks != 4 || h2.TaskTokens != 250_000 {
		t.Fatalf("typical task without own history = %+v", h2)
	}
	if h3 := Learn(nil, ""); h3.TaskTokens != DefaultTaskTokens || h3.TaskDuration != DefaultTaskDuration {
		t.Fatalf("defaults = %+v", h3)
	}
	if got := h.Task("x"); got.Tokens != 200_000 || got.Text != "x" {
		t.Fatalf("Task = %+v", got)
	}
}

func TestProviderFromLogsAndTracker(t *testing.T) {
	t0 := at(20, 0)
	recs := []sessionlog.Record{
		quotaRec(t0, "claude", event.QuotaInfo{Utilization: 0.4, Window: "five_hour", ResetsAt: t0.Add(2 * time.Hour)}),
		// Codex: the weekly window is the fullest; its 5h window is at 30%.
		quotaRec(t0, "codex", event.QuotaInfo{Utilization: 0.5, Window: "7d", ResetsAt: t0.Add(72 * time.Hour), Windows: map[string]float64{"5h": 0.3, "7d": 0.5}}),
	}
	h := Learn(recs, "")
	now := t0.Add(time.Hour)
	p := h.Provider("claude", nil, now, 0.9)
	if p.Used != 0.4 || !p.ResetsAt.Equal(t0.Add(2*time.Hour)) || !strings.Contains(p.Note, "40% used") || !strings.Contains(p.Note, "(read 20:00)") {
		t.Fatalf("claude = %+v", p)
	}
	c := h.Provider("codex", nil, now, 0.9)
	if c.Used != 0.3 || !c.ResetsAt.Equal(t0.Add(WindowLen)) || !c.LimitedUntil.IsZero() {
		t.Fatalf("codex = %+v", c)
	}
	// After the window: nothing in use.
	late := h.Provider("claude", nil, t0.Add(3*time.Hour), 0.9)
	if late.Used != 0 || !late.ResetsAt.IsZero() || !strings.Contains(late.Note, "has reset") {
		t.Fatalf("claude later = %+v", late)
	}
	// A weekly window over the ceiling is a limit until it resets.
	recs = append(recs, quotaRec(t0.Add(time.Minute), "codex", event.QuotaInfo{Utilization: 0.95, Window: "7d", ResetsAt: t0.Add(72 * time.Hour)}))
	c = Learn(recs, "").Provider("codex", nil, now, 0.9)
	if !c.LimitedUntil.Equal(t0.Add(72*time.Hour)) || !strings.Contains(c.Note, "weekly window 95%") {
		t.Fatalf("codex weekly = %+v", c)
	}
	// A limit record, and a logged-out CLI that is no limit.
	until := now.Add(30 * time.Minute)
	away := now.Add(10 * time.Hour)
	recs = append(recs,
		sessionlog.Record{Type: sessionlog.TypeLimit, TS: t0, Provider: "claude", Until: &until, Unavailable: sessionlog.Bool(false)},
		sessionlog.Record{Type: sessionlog.TypeLimit, TS: t0.Add(time.Minute), Provider: "gemini", Until: &away, Unavailable: sessionlog.Bool(true)})
	h = Learn(recs, "")
	if p := h.Provider("claude", nil, now, 0.9); !p.LimitedUntil.Equal(until) || !strings.Contains(p.Note, "at its limit") {
		t.Fatalf("claude limited = %+v", p)
	}
	if p := h.Provider("gemini", nil, now, 0.9); !p.LimitedUntil.IsZero() {
		t.Fatalf("a logged-out CLI is not limited: %+v", p)
	}
	// The live tracker wins over the logs.
	tr := limits.NewTracker()
	tr.SetQuota("claude", event.QuotaInfo{Utilization: 0.7, Window: "five_hour", ResetsAt: now.Add(time.Hour)})
	if p := h.Provider("claude", tr, now, 0.9); p.Used != 0.7 || strings.Contains(p.Note, "read") {
		t.Fatalf("claude live = %+v", p)
	}
	// No reading at all.
	if p := Learn(nil, "").Provider("claude", nil, now, 0); p.Used != 0 || p.Note != "" || !p.ResetsAt.IsZero() {
		t.Fatalf("no reading = %+v", p)
	}
}
