package orchestrator

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparkz400/relayweft/internal/config"
	"github.com/sparkz400/relayweft/internal/event"
	"github.com/sparkz400/relayweft/internal/sessionlog"
)

// teamFolder sets up a team folder with another machine that spent $0.95
// today, a corrupt file and a stale copy of this machine's own file (which
// must not count: this machine's own day comes from its logs). It returns
// the folder and this machine's id.
func teamFolder(t *testing.T) (string, string) {
	t.Helper()
	old := sessionlog.MachineIDFile
	idFile := filepath.Join(t.TempDir(), "machine-id")
	sessionlog.MachineIDFile = func() string { return idFile }
	t.Cleanup(func() { sessionlog.MachineIDFile = old })
	me, err := sessionlog.MachineID()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := time.Now()
	other := sessionlog.Export{Format: sessionlog.ExportFormat, Version: sessionlog.ExportVersion, Machine: "aaaabbbbccccdddd", Generated: now,
		Days: []sessionlog.ExportDay{{Date: now.Local().Format("2006-01-02"), Tasks: 3, FreshTokens: 9000, USD: 0.95}}}
	if err := sessionlog.WriteTeamFile(dir, other); err != nil {
		t.Fatal(err)
	}
	mine := other
	mine.Machine = me
	mine.Days = []sessionlog.ExportDay{{Date: now.Local().Format("2006-01-02"), USD: 500}}
	if err := sessionlog.WriteTeamFile(dir, mine); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "eeeeffff00001111.json"), []byte("{not json"), 0o644)
	return dir, me
}

// The combined day of every machine reaches the team limit: an attended
// task is asked through ApproveBudget, an unattended one stops. A corrupt
// file is skipped with a warning, and after the task this machine writes
// its own file, and only that.
func TestTeamBudgetAsksAndStops(t *testing.T) {
	for _, attended := range []bool{true, false} {
		dir, me := teamFolder(t)
		others := map[string][]byte{}
		files, _ := filepath.Glob(filepath.Join(dir, "*"))
		for _, f := range files {
			if filepath.Base(f) != me+".json" {
				others[f], _ = os.ReadFile(f)
			}
		}
		var ran sync.Map
		o, rec := newOrc(t, "", budgetSet(&ran), budgetCfg(func(b *config.BudgetCfg) {
			b.Team = config.TeamBudgetCfg{Dir: dir, DayUSD: 1}
		}))
		ap := &fakeApprover{budget: func(BudgetRequest) bool { return false }}
		withApprover(o, ap)
		res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: !attended})
		if res.OK || !strings.Contains(res.Summary, "the team's cost today $1.04 reached the budget of $1.00") {
			t.Fatalf("attended=%v: %+v", attended, res)
		}
		if n := steps(&ran); n != 0 {
			t.Errorf("attended=%v: %d steps ran after the planner crossed the team limit", attended, n)
		}
		if attended && (len(ap.budgets) != 1 || ap.budgets[0].Limit != LimitTeamDayUSD || !ap.budgets[0].USD()) {
			t.Errorf("asked %+v", ap.budgets)
		}
		if !attended && len(ap.budgets) != 0 {
			t.Errorf("an unattended task asked: %+v", ap.budgets)
		}
		evs := rec.all()
		if n := countLogs(evs, event.Log, "budget: team folder "+dir+": skipped eeeeffff00001111.json"); n != 1 {
			t.Errorf("attended=%v: %d warnings for the corrupt file, want 1", attended, n)
		}
		found := false
		for _, e := range evs {
			if e.Kind == event.Error && strings.Contains(e.Text, "budget.team.day_usd") {
				found = true
			}
		}
		if !found {
			t.Error("no hint how to raise the team budget")
		}
		// This machine's own file now holds this task, and nothing else
		// changed or appeared.
		e, err := sessionlog.ReadExport(sessionlog.TeamFile(dir, me))
		if err != nil {
			t.Fatal(err)
		}
		tok, usd := e.DayTotal(time.Now().Local().Format("2006-01-02"))
		if e.Machine != me || tok != 1000 || usd < 0.089 || usd > 0.091 || len(e.Tasks) != 0 {
			t.Errorf("own file: %+v", e)
		}
		after, _ := filepath.Glob(filepath.Join(dir, "*"))
		if len(after) != len(files) {
			t.Errorf("files before %v, after %v", files, after)
		}
		for f, data := range others {
			if now, _ := os.ReadFile(f); !bytes.Equal(now, data) {
				t.Errorf("another machine's file changed: %s", f)
			}
		}
	}
}

// Below the team limit nothing asks; a team folder that is gone only warns.
func TestTeamBudgetMissingFolderWarns(t *testing.T) {
	dir, _ := teamFolder(t)
	var ran sync.Map
	o, rec := newOrc(t, "", budgetSet(&ran), budgetCfg(func(b *config.BudgetCfg) {
		b.Team = config.TeamBudgetCfg{Dir: filepath.Join(dir, "gone"), DayUSD: 5}
	}))
	res := o.RunWith(context.Background(), longTask, TaskOptions{Unattended: true})
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if n := countLogs(rec.all(), event.Log, "budget: cannot read the team folder"); n != 1 {
		t.Errorf("%d warnings for the missing folder, want 1", n)
	}
	// The write after the task creates the folder with this machine's file.
	if _, err := os.Stat(filepath.Join(dir, "gone")); err != nil {
		t.Errorf("own file not written: %v", err)
	}
}

func TestTeamBudgetRequestText(t *testing.T) {
	r := BudgetRequest{Limit: LimitTeamDayTokens, Used: 2_000_000, Max: 1_000_000}
	if r.String() != "the team's tokens today 2.0M tokens reached the budget of 1.0M tokens" || r.Flag() != "" || !strings.Contains(r.RaiseHint(), "budget.team.day_tokens") {
		t.Errorf("%q %q %q", r.String(), r.Flag(), r.RaiseHint())
	}
}
