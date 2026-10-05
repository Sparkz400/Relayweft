package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/health"
	"github.com/sparkz400/relayweft/internal/sessionlog"
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
	// Two other machines in the team folder: one writing now (its file
	// holds the last 7 days, as rw writes it), one that stopped 20 days ago.
	day := func(ago int) string { return time.Now().AddDate(0, 0, -ago).Format("2006-01-02") }
	other := sessionlog.Export{Format: sessionlog.ExportFormat, Version: sessionlog.ExportVersion, Machine: "othermachine", Generated: time.Now(),
		Since: sessionlog.DayStart(time.Now()).AddDate(0, 0, -6), Days: []sessionlog.ExportDay{{Date: day(0), Tasks: 3, FreshTokens: 9000, USD: 2}}}
	stale := sessionlog.Export{Format: sessionlog.ExportFormat, Version: sessionlog.ExportVersion, Machine: "stalemachine", Generated: time.Now().AddDate(0, 0, -20),
		Since: sessionlog.DayStart(time.Now()).AddDate(0, 0, -26), Days: []sessionlog.ExportDay{{Date: day(22), Tasks: 1, FreshTokens: 500, USD: 5}}}
	for _, e := range []sessionlog.Export{other, stale} {
		if err := sessionlog.WriteTeamFile(team, e); err != nil {
			t.Fatal(err)
		}
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
	b := v.Budget
	if b.DayUSD != 5 || !b.Team || b.TeamDayUSD != 20 || b.TeamMachines != 2 || len(b.TeamDays) != 30 {
		t.Errorf("budget = %+v (warnings %v)", b, v.Warnings)
	}
	// Each machine counts only for the days its file holds; the rest is
	// "no data", not zero use. The stopped machine is no warning.
	for ago, want := range map[int]teamDay{
		0:  {USD: 2, Tokens: 9000, Machines: 1, NoData: 1},
		10: {Machines: 0, NoData: 2},
		22: {USD: 5, Tokens: 500, Machines: 1, NoData: 1},
		29: {Machines: 0, NoData: 2},
	} {
		if got := b.TeamDays[day(ago)]; got != want {
			t.Errorf("team day %d days ago = %+v, want %+v", ago, got, want)
		}
	}
	if len(v.Warnings) != 0 {
		t.Errorf("warnings = %v", v.Warnings)
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

// The dashboard is built once and served again: by a second request, by
// one that came while it was built, until a task ends here or the panel
// asks for a fresh one.
func TestDashboardEndpointCaches(t *testing.T) {
	healthOptions = health.Options{Dir: t.TempDir(), NoLeftovers: true}
	defer func() { healthOptions = health.Options{} }()
	env := newEnv(t, func(c *config.Config) { c.LogDir = t.TempDir() })
	cached := func(q string) bool {
		var v struct {
			Cached bool `json:"cached"`
		}
		env.call("GET", "/api/dashboard"+q, nil, &v)
		return v.Cached
	}
	if cached("") || !cached("") || cached("?days=30") || !cached("?days=30") {
		t.Error("a repeated request was built again, or a new one was cached")
	}
	if cached("?fresh=1") || !cached("") {
		t.Error("fresh=1 did not rebuild")
	}
	env.srv.observe(event.Event{Kind: event.TaskDone, OK: true})
	if cached("") {
		t.Error("served from the cache after a task ended")
	}
}

func TestDashCache(t *testing.T) {
	var c dashCache
	clock := time.Now()
	c.now = func() time.Time { return clock }
	k := dashKey{7, false}
	release := make(chan struct{})
	builds := 0
	build := func(dashKey) (*dashboardView, error) {
		builds++
		<-release
		return &dashboardView{RangeDays: builds}, nil
	}
	// Three requests while the first build runs share it; one that gives
	// up returns at once, and the build still finishes.
	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() { _, err := c.get(ctx, k, false, build); gaveUp <- err }()
	results := make(chan *dashboardView, 2)
	for i := 0; i < 2; i++ {
		go func() { v, _ := c.get(context.Background(), k, false, build); results <- v }()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled request: %v", err)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if v := <-results; v.RangeDays != 1 || v.Cached {
			t.Errorf("shared build: %+v", v)
		}
	}
	if v, _ := c.get(context.Background(), k, false, build); builds != 1 || !v.Cached {
		t.Errorf("second request: %d builds, %+v", builds, v)
	}
	clock = clock.Add(dashTTL)
	if v, _ := c.get(context.Background(), k, false, build); builds != 2 || v.Cached {
		t.Errorf("after the TTL: %d builds", builds)
	}
	c.clear()
	if c.get(context.Background(), k, false, build); builds != 3 {
		t.Errorf("after clear: %d builds", builds)
	}
	// A failed build is not served again.
	failing := func(dashKey) (*dashboardView, error) { builds++; return nil, errors.New("boom") }
	c.get(context.Background(), dashKey{30, true}, false, failing)
	if _, err := c.get(context.Background(), dashKey{30, true}, false, failing); err == nil || builds != 5 {
		t.Errorf("failed build cached: %v, %d builds", err, builds)
	}
}

// /api/stats suggests what rw tune and the dashboard suggest: with the
// models of your config, not the built-in defaults.
func TestStatsSuggestionsUseConfig(t *testing.T) {
	logs := t.TempDir()
	var buf bytes.Buffer
	for i := 0; i < 5; i++ {
		b, _ := json.Marshal(sessionlog.Record{Type: sessionlog.TypeAgentEnd, TS: time.Now(), Session: "s", TaskID: "t", Step: "x", Attempt: 1,
			Role: "researcher", Provider: "codex", Model: "gpt-big", OK: sessionlog.Bool(true), Tokens: &event.TokenUsage{Input: 1000}})
		buf.Write(append(b, '\n'))
	}
	os.WriteFile(filepath.Join(logs, "s.jsonl"), buf.Bytes(), 0o644)
	env := newEnv(t, func(c *config.Config) {
		c.LogDir = logs
		ex := c.Roles["explorer"]
		ex.Codex = config.Route{Model: "mini-test", Effort: "low"}
		c.Roles["explorer"] = ex
	})
	var sv statsView
	env.call("GET", "/api/stats", nil, &sv)
	if len(sv.Suggestions) != 1 || fmt.Sprint(sv.Suggestions[0].Commands) != "[/route researcher codex:mini-test:low]" {
		t.Errorf("suggestions = %+v", sv.Suggestions)
	}
}
