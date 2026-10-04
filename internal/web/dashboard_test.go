package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparkz400/switchyard/internal/config"
	"github.com/sparkz400/switchyard/internal/event"
	"github.com/sparkz400/switchyard/internal/health"
	"github.com/sparkz400/switchyard/internal/sessionlog"
)

// dashLog writes a session log with two tasks today, one of them carrying
// markup in its texts (log content is untrusted).
func dashLog(t *testing.T, dir string) {
	t.Helper()
	now := time.Now()
	recs := []sessionlog.Record{
		{Type: sessionlog.TypeAgentEnd, TS: now, Session: "s", TaskID: "a", Step: "s1", Attempt: 1, Role: "worker", Provider: "codex", Model: "<img src=x onerror=alert(1)>",
			OK: sessionlog.Bool(true), Tokens: &event.TokenUsage{Input: 1000}},
		{Type: sessionlog.TypeTaskEnd, TS: now, Session: "s", TaskID: "a", OK: sessionlog.Bool(true), Text: "<script>alert(1)</script>",
			Cost: &event.TaskCost{CostUSD: 1.5, PerProvider: map[string]event.TokenUsage{event.Codex: {Input: 1000}}}},
		{Type: sessionlog.TypeTaskEnd, TS: now, Session: "s", TaskID: "b", OK: sessionlog.Bool(false), Text: "cancelled: by you"},
		{Type: sessionlog.TypeLimit, TS: now, Session: "s", Provider: "claude", Text: "<b>limit</b>"},
	}
	var buf bytes.Buffer
	for _, r := range recs {
		b, _ := json.Marshal(r)
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(dir, "20261004-090000-abcd.jsonl"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardEndpoint(t *testing.T) {
	logs, team := t.TempDir(), t.TempDir()
	dashLog(t, logs)
	healthOptions = health.Options{Dir: t.TempDir(), NoLeftovers: true}
	defer func() { healthOptions = health.Options{} }()
	idFile := filepath.Join(t.TempDir(), "machine-id")
	defer func(f func() string) { sessionlog.MachineIDFile = f }(sessionlog.MachineIDFile)
	sessionlog.MachineIDFile = func() string { return idFile }
	// Another machine's export in the team folder.
	other := sessionlog.Export{Format: sessionlog.ExportFormat, Version: sessionlog.ExportVersion, Machine: "othermachine", Generated: time.Now(),
		Days: []sessionlog.ExportDay{{Date: time.Now().Format("2006-01-02"), Tasks: 3, FreshTokens: 9000, USD: 2}}}
	if err := sessionlog.WriteTeamFile(team, other); err != nil {
		t.Fatal(err)
	}
	env := newEnv(t, func(c *config.Config) {
		c.LogDir = logs
		c.Budget.DayUSD = 5
		c.Budget.Team = config.TeamBudgetCfg{Dir: team, DayUSD: 20}
	})
	env.srv.orc.Tracker().SetQuota("claude", event.QuotaInfo{Utilization: 0.4, Window: "seven_day", ResetsAt: time.Now().Add(time.Hour),
		Windows: map[string]float64{"five_hour": 0.1, "seven_day": 0.4}})

	// The guard applies: no session, a foreign host, a foreign page.
	for name, hdr := range map[string]map[string]string{
		"no session":     {SessionHeader: ""},
		"foreign host":   {"Host": "evil.example:" + portOf(env.srv.Addr())},
		"foreign origin": {"Origin": "http://evil.example"},
		"cross-site":     {"Sec-Fetch-Site": "cross-site"},
	} {
		res, _ := env.do("GET", "/api/dashboard", nil, hdr)
		if res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	for _, bad := range []string{"0", "-1", "367", "x"} {
		if res, _ := env.do("GET", "/api/dashboard?days="+bad, nil, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("days=%s: %d", bad, res.StatusCode)
		}
	}

	res, data := env.do("GET", "/api/dashboard?days=30", nil, nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	// Markup from the logs reaches the page only as escaped JSON text.
	for _, raw := range []string{"<script>", "<img", "<b>"} {
		if bytes.Contains(data, []byte(raw)) {
			t.Errorf("response contains raw %q", raw)
		}
	}
	var v struct {
		RangeDays int                       `json:"range_days"`
		Days      []sessionlog.DashDay      `json:"days"`
		Totals    sessionlog.DashTotals     `json:"totals"`
		Routes    []sessionlog.RouteRow     `json:"routes"`
		Limits    sessionlog.DashLimits     `json:"limits"`
		Budget    dashBudget                `json:"budget"`
		Quota     map[string]dashQuota      `json:"quota"`
		Learned   *dashLearned              `json:"learned"`
		Health    *dashHealth               `json:"health"`
		Warnings  []string                  `json:"warnings"`
		Events    []sessionlog.LearnedEvent `json:"learned_events"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v.RangeDays != 30 || len(v.Days) != 30 || v.Totals.Tasks != 2 || v.Totals.OK != 1 || v.Totals.Cancelled != 1 || v.Totals.USD != 1.5 {
		t.Errorf("range %d, %d days, totals %+v", v.RangeDays, len(v.Days), v.Totals)
	}
	if len(v.Routes) != 1 || v.Routes[0].Model != "<img src=x onerror=alert(1)>" || v.Limits.PerProvider["claude"].Hits != 1 {
		t.Errorf("routes %+v, limits %+v", v.Routes, v.Limits)
	}
	today := time.Now().Format("2006-01-02")
	if b := v.Budget; b.DayUSD != 5 || !b.Team || b.TeamDayUSD != 20 || b.TeamDays[today].USD != 2 {
		t.Errorf("budget = %+v (warnings %v)", b, v.Warnings)
	}
	if q := v.Quota["claude"]; q.Source != "live" || q.Windows["five_hour"] != 0.1 || q.ResetsAt == nil {
		t.Errorf("quota = %+v", v.Quota)
	}
	if v.Learned == nil || v.Learned.Note == "" || v.Learned.Routes == nil || v.Learned.Pending == nil {
		t.Errorf("learned = %+v", v.Learned)
	}
	if v.Health == nil || v.Health.Criterion.NeedDays != 14 || v.Health.UseDays == nil {
		t.Errorf("health = %+v", v.Health)
	}
	if v.Warnings == nil || v.Events == nil {
		t.Errorf("nil lists: warnings %v, learned events %v", v.Warnings, v.Events)
	}
}

// A fresh install: no logs at all is an empty dashboard, not an error.
func TestDashboardFreshInstall(t *testing.T) {
	healthOptions = health.Options{Dir: t.TempDir(), NoLeftovers: true}
	defer func() { healthOptions = health.Options{} }()
	env := newEnv(t, func(c *config.Config) { c.LogDir = filepath.Join(t.TempDir(), "never-created") })
	var v dashboardView
	env.call("GET", "/api/dashboard", nil, &v)
	if v.RangeDays != 7 || len(v.Days) != 7 || v.Totals.Tasks != 0 || len(v.Quota) != 0 || v.Health == nil || v.Health.Criterion.Met {
		t.Errorf("fresh install: %+v", v)
	}
}

// The page builds everything from data with text nodes: the only HTML
// sink is icon(), which takes the static ICONS. Log content (prompts,
// model output, model names) must never reach another one.
func TestPageHasNoHTMLSinks(t *testing.T) {
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, sink := range []string{"insertAdjacentHTML", "outerHTML", "document.write", "eval(", "new Function"} {
		if bytes.Contains(js, []byte(sink)) {
			t.Errorf("app.js uses %s", sink)
		}
	}
	if n := bytes.Count(js, []byte("innerHTML")); n != 1 || !bytes.Contains(js, []byte("s.innerHTML = ICONS[name] || '';")) {
		t.Errorf("app.js has %d innerHTML uses, want only icon()'s", n)
	}
	page, _ := staticFS.ReadFile("static/index.html")
	if bytes.Count(page, []byte("<script")) != bytes.Count(page, []byte("<script src=")) {
		t.Error("index.html has an inline script (the CSP blocks it)")
	}
}
