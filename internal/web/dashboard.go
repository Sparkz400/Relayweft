package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/health"
	"github.com/sparkz400/relayweft/internal/orchestrator"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// The Dashboard panel: GET /api/dashboard?days=7|30|90[&here=1]. The
// session-log part is sessionlog.BuildDashboard (the counting of `rw stats`
// and `rw tune`); this adds what lives elsewhere: the budget, the team
// folder, the current quota, this repo's learned routes and the health
// streak. Fields only ever get added (the JetBrains client reads it too).

// dashMaxDays caps the range; dashLearnDays is how far back the dry run of
// `rw tune --apply` looks (older runs weigh 1/8 or less there anyway).
const (
	dashMaxDays   = 366
	dashLearnDays = 90
)

type dashboardView struct {
	sessionlog.Dashboard
	Generated     time.Time            `json:"generated"`
	RangeDays     int                  `json:"range_days"`
	Here          bool                 `json:"here"`
	Dir           string               `json:"dir"`
	LogDir        string               `json:"log_dir"`
	MinTasks      int                  `json:"min_tasks"` // below it suggestions are hints (rw tune)
	Budget        dashBudget           `json:"budget"`
	Quota         map[string]dashQuota `json:"quota"` // current use of each provider's limit
	LearnedRoutes *dashLearned         `json:"learned"`
	Health        *dashHealth          `json:"health"`
	Warnings      []string             `json:"warnings"`
	ElapsedMS     int64                `json:"elapsed_ms"`
	Cached        bool                 `json:"cached"` // answered from the cache (see dashCache)
}

type dashBudget struct {
	DayUSD        float64 `json:"day_usd,omitempty"`
	DayTokens     int64   `json:"day_tokens,omitempty"`
	TeamDayUSD    float64 `json:"team_day_usd,omitempty"`
	TeamDayTokens int64   `json:"team_day_tokens,omitempty"`
	Team          bool    `json:"team"` // a team folder is set
	// TeamMachines is how many other machines have a file in the team
	// folder; TeamDays are their totals per date of the range. A machine's
	// file holds only its last days (7 when rw writes it), so for older
	// days it has no data.
	TeamMachines int                `json:"team_machines,omitempty"`
	TeamDays     map[string]teamDay `json:"team_days,omitempty"`
}

type teamDay struct {
	Tokens   int64   `json:"fresh_tokens"`
	USD      float64 `json:"usd"`
	Machines int     `json:"machines"` // machines whose file holds this day
	NoData   int     `json:"no_data"`  // machines whose file does not
}

type dashQuota struct {
	Source       string             `json:"source"` // live (this rw) or log (the newest logged reading)
	Seen         time.Time          `json:"seen"`
	Utilization  float64            `json:"utilization"`
	Window       string             `json:"window,omitempty"`
	ResetsAt     *time.Time         `json:"resets_at,omitempty"`
	Status       string             `json:"status,omitempty"`
	Windows      map[string]float64 `json:"windows,omitempty"` // e.g. five_hour, seven_day
	LimitedUntil *time.Time         `json:"limited_until,omitempty"`
}

type dashLearned struct {
	Root    string           `json:"root,omitempty"`
	Mode    string           `json:"mode"` // routing.learn
	Updated *time.Time       `json:"updated,omitempty"`
	Routes  []learnedRow     `json:"routes"`
	Pending []learnedPending `json:"pending"` // what `rw tune --apply` would change
	Note    string           `json:"note,omitempty"`
}

type learnedRow struct {
	Role     string                 `json:"role"`
	Route    string                 `json:"route"`
	Since    time.Time              `json:"since"`
	Why      string                 `json:"why"`
	InUse    bool                   `json:"in_use"`
	Evidence []config.RouteEvidence `json:"evidence"`
}

type learnedPending struct {
	Role   string `json:"role"`
	From   string `json:"from"`
	To     string `json:"to"`
	Remove bool   `json:"remove,omitempty"`
	Why    string `json:"why"`
}

type dashHealth struct {
	Criterion health.Criterion `json:"criterion"`
	Days      int              `json:"days"`
	Now       time.Time        `json:"now"`
	UseDays   []string         `json:"use_days"`
	BadDays   map[string]int   `json:"bad_days"` // crashes and hangs per date
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := 7
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > dashMaxDays {
			fail(w, http.StatusBadRequest, fmt.Errorf("days %q: want 1-%d", v, dashMaxDays))
			return
		}
		days = n
	}
	v, err := s.dash.get(r.Context(), dashKey{days, q.Get("here") == "1"}, q.Get("fresh") == "1",
		func(k dashKey) (*dashboardView, error) { return s.dashboard(k.days, k.here, time.Now()) })
	if err != nil {
		if r.Context().Err() != nil {
			return // the page went away or asked again
		}
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, v)
}

// dashTTL is how long a built dashboard is served again. Every load reads
// up to 90 days of logs, the team folder and the health logs, and asks the
// repo for its root, so quick range clicks and the start page's summary
// share one build. A task ending here clears the cache; the panel's
// Refresh asks for a fresh one.
const dashTTL = time.Minute

type dashKey struct {
	days int
	here bool
}

// dashCache serves built dashboards for dashTTL and runs one build at a
// time: a request that arrives while the same one is being built waits
// for it. A build runs on when its request goes away, so the next click
// gets it.
type dashCache struct {
	mu      sync.Mutex
	gen     int // bumped by clear
	entries map[dashKey]*dashEntry
	build   sync.Mutex // one build at a time, whatever the key
	now     func() time.Time
}

type dashEntry struct {
	done chan struct{}
	v    *dashboardView
	err  error
	at   time.Time // zero while building
	gen  int
}

// clear drops what was built before (a task ended: its numbers changed).
func (c *dashCache) clear() {
	c.mu.Lock()
	c.gen++
	c.mu.Unlock()
}

func (c *dashCache) get(ctx context.Context, k dashKey, fresh bool, build func(dashKey) (*dashboardView, error)) (*dashboardView, error) {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[dashKey]*dashEntry{}
	}
	e := c.entries[k]
	building := e != nil && e.at.IsZero()
	if e == nil || (!building && (fresh || e.err != nil || e.gen != c.gen || now().Sub(e.at) >= dashTTL)) {
		e = &dashEntry{done: make(chan struct{}), gen: c.gen}
		c.entries[k] = e
		go func() {
			c.build.Lock()
			v, err := build(k)
			c.build.Unlock()
			c.mu.Lock()
			e.v, e.err, e.at = v, err, now()
			c.mu.Unlock()
			close(e.done)
		}()
		building = true
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.done:
	}
	if e.err != nil {
		return nil, e.err
	}
	v := *e.v // the cached view is shared: mark a copy
	v.Cached = !building
	return &v, nil
}

// dashboard builds the view for the last days days (today included).
func (s *Server) dashboard(days int, here bool, now time.Time) (*dashboardView, error) {
	began := time.Now()
	cfg := s.store.Get()
	dir := cfg.SessionDir()
	v := &dashboardView{Generated: now, RangeDays: days, Here: here, Dir: s.opt.Dir, LogDir: dir, MinTasks: minTuneTasks,
		Quota: map[string]dashQuota{}, Warnings: []string{}}
	// One read covers the range and the learned-routes dry run; files not
	// written to since then are skipped unread.
	readFrom := sessionlog.SuggestSince(now, max(days, dashLearnDays))
	recs, err := sessionlog.ReadDirSince(dir, readFrom)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		if len(recs) == 0 {
			return nil, err
		}
		v.Warnings = append(v.Warnings, "some session logs could not be read: "+err.Error())
	}
	o := sessionlog.DashboardOptions{Days: days, Now: now, Catalog: sessionlog.CatalogFrom(cfg)}
	if here {
		o.Cwd = s.opt.Dir
	}
	v.Dashboard = sessionlog.BuildDashboard(recs, o)
	v.Budget = s.dashBudget(cfg, now, v)
	v.quota(s, cfg, now)
	v.LearnedRoutes = s.dashLearned(cfg, recs, now)
	v.Health = s.dashHealth(v)
	v.ElapsedMS = time.Since(began).Milliseconds()
	return v, nil
}

// dashBudget is the daily and team budget, with the other machines' days.
func (s *Server) dashBudget(cfg *config.Config, now time.Time, v *dashboardView) dashBudget {
	b := dashBudget{DayUSD: cfg.Budget.DayUSD, DayTokens: cfg.Budget.DayTokens}
	tb := cfg.Budget.Team
	if tb.Dir == "" {
		return b
	}
	b.Team, b.TeamDayUSD, b.TeamDayTokens = true, tb.DayUSD, tb.DayTokens
	folder, err := tb.Folder()
	if err != nil {
		v.Warnings = append(v.Warnings, "team budget: "+err.Error())
		return b
	}
	me, err := sessionlog.MachineID()
	if err != nil {
		v.Warnings = append(v.Warnings, "team budget: no machine id ("+err.Error()+"); this machine's own file may be counted twice")
	}
	// Stale files are kept: the days they hold are still true, and a
	// machine that stopped writing must not warn on every load.
	exps, warns, err := sessionlog.ReadTeamHistory(folder, me, now)
	if err != nil {
		v.Warnings = append(v.Warnings, "team budget: cannot read the team folder "+folder+": "+err.Error())
		return b
	}
	for _, w := range warns {
		v.Warnings = append(v.Warnings, "team folder: "+w)
	}
	b.TeamMachines = len(exps)
	b.TeamDays = map[string]teamDay{}
	for _, d := range v.Days {
		var td teamDay
		for _, e := range exps {
			if !e.Covers(d.Date) {
				td.NoData++
				continue
			}
			td.Machines++
			tok, usd := e.DayTotal(d.Date)
			td.Tokens += tok
			td.USD += usd
		}
		b.TeamDays[d.Date] = td
	}
	return b
}

// quota fills the current use of each provider's limit: this rw's live
// reading, else the newest logged one whose window has not reset yet.
func (v *dashboardView) quota(s *Server, cfg *config.Config, now time.Time) {
	tr := s.orc.Tracker()
	for _, p := range cfg.ProviderNames() {
		st := tr.Snapshot(p)
		var dq dashQuota
		var qi *dashQuota
		if _, ok := tr.Utilization(p); ok && st.Quota != nil {
			dq = quotaView(*st.Quota, "live", now)
			qi = &dq
		} else if lq, ok := v.Limits.Quota[p]; ok && (lq.Quota.ResetsAt.IsZero() || now.Before(lq.Quota.ResetsAt)) {
			dq = quotaView(lq.Quota, "log", lq.TS)
			qi = &dq
		}
		if st.Limited(now) {
			if qi == nil {
				dq = dashQuota{Source: "live", Seen: now}
				qi = &dq
			}
			u := st.LimitedUntil
			qi.LimitedUntil = &u
		}
		if qi != nil {
			v.Quota[p] = *qi
		}
	}
}

func quotaView(q event.QuotaInfo, source string, seen time.Time) dashQuota {
	dq := dashQuota{Source: source, Seen: seen, Utilization: q.Utilization, Window: q.Window, Status: q.Status, Windows: q.Windows}
	if !q.ResetsAt.IsZero() {
		t := q.ResetsAt
		dq.ResetsAt = &t
	}
	if len(dq.Windows) == 0 && q.Window != "" {
		dq.Windows = map[string]float64{q.Window: q.Utilization}
	}
	return dq
}

// dashLearned is this repo's learned routes and what `rw tune --apply`
// would change now.
func (s *Server) dashLearned(cfg *config.Config, recs []sessionlog.Record, now time.Time) *dashLearned {
	l := &dashLearned{Mode: cfg.LearnMode(), Routes: []learnedRow{}, Pending: []learnedPending{}}
	if s.opt.Demo {
		l.Note = "demo mode: no repo, no learned routes"
		return l
	}
	root, err := orchestrator.LearnedRoot(s.opt.Dir)
	if err != nil {
		l.Note = err.Error()
		return l
	}
	l.Root = root
	stored, err := config.LoadLearned(root)
	if err != nil {
		l.Note = "learned routes: " + err.Error()
		return l
	}
	if !stored.Updated.IsZero() {
		u := stored.Updated
		l.Updated = &u
	}
	live := s.store.Learned()
	for role, lr := range stored.Routes {
		cur, ok := live[role]
		l.Routes = append(l.Routes, learnedRow{Role: role, Route: lr.Spec(), Since: lr.Since, Why: lr.Why, Evidence: lr.Evidence,
			InUse: ok && cur.Spec() == lr.Spec()})
	}
	sort.Slice(l.Routes, func(i, j int) bool { return l.Routes[i].Role < l.Routes[j].Role })
	rep, err := orchestrator.UpdateLearned(s.opt.Dir, s.store.Unlearned(), recs, now, true)
	if err != nil {
		l.Note = "rw tune --apply dry run: " + err.Error()
		return l
	}
	for _, c := range rep.Result.Changes {
		l.Pending = append(l.Pending, learnedPending{Role: c.Role, From: c.From.String(), To: c.To.String(), Remove: c.Remove, Why: c.Why})
	}
	return l
}

// dashHealth is the clean streak of `rw health`. The scan for leftovers is
// skipped: it walks the worktree pools, and the Health panel has it.
func (s *Server) dashHealth(v *dashboardView) *dashHealth {
	o := healthOptions
	o.NoLeftovers = true
	rep, err := health.Build(o)
	if err != nil {
		v.Warnings = append(v.Warnings, "health: "+err.Error())
		return nil
	}
	h := &dashHealth{Criterion: rep.Criterion, Days: rep.Days, Now: rep.Now, UseDays: rep.UseDays, BadDays: map[string]int{}}
	if h.UseDays == nil {
		h.UseDays = []string{}
	}
	for _, in := range append(append([]health.Incident{}, rep.Crashes...), rep.Hangs...) {
		h.BadDays[in.Time.Local().Format("2006-01-02")]++
	}
	return h
}
