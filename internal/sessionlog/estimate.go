package sessionlog

import (
	"sort"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// Dry-run estimates: what a planned step will likely use, from past runs
// of the same role, kind and route. The history is tried in order:
//
//  1. this repo (at least minEstimateSamples runs),
//  2. all repos,
//  3. a fixed default by kind, marked "no history".
//
// Each estimate is the median with the 25th-75th percentile as its range.
// The $ is Claude's API-equivalent price as Claude Code reports it (the
// figure stats and budgets use); Codex reports none, so its steps cost $0.

// Estimate sources.
const (
	SourceRepo = "this repo"
	SourceAll  = "all repos"
	SourceNone = "no history"
)

// minEstimateSamples is how many runs a history level needs to be used.
const minEstimateSamples = 3

// Defaults without history, per run.
const (
	defaultReadTokens  = 20_000
	defaultWriteTokens = 60_000
	defaultReadWall    = 90 * time.Second
	defaultWriteWall   = 5 * time.Minute
	// defaultClaudeUSDPerToken prices fresh Claude tokens when no run with a
	// reported price exists to take the rate from.
	defaultClaudeUSDPerToken = 6.0 / 1e6
)

// Spread is a value's typical range: the 25th percentile, the median and
// the 75th percentile.
type Spread struct {
	Low  float64 `json:"low"`
	Mid  float64 `json:"mid"`
	High float64 `json:"high"`
}

// Add sums two spreads (an upper-bound style total: the ranges add up).
func (s Spread) Add(o Spread) Spread { return Spread{s.Low + o.Low, s.Mid + o.Mid, s.High + o.High} }

// Max takes the larger of two spreads per point (parallel steps).
func (s Spread) Max(o Spread) Spread {
	return Spread{max(s.Low, o.Low), max(s.Mid, o.Mid), max(s.High, o.High)}
}

// Estimate is what one step will likely use.
type Estimate struct {
	Tokens  Spread `json:"tokens"`  // fresh tokens
	Seconds Spread `json:"seconds"` // wall time
	USD     Spread `json:"usd"`     // API-equivalent $ (Claude only)
	Samples int    `json:"samples"` // runs it is based on (0 = no history)
	Source  string `json:"source"`  // SourceRepo, SourceAll or SourceNone
}

// History indexes past routed runs for estimates. Build it once per plan
// approval: Estimate is cheap.
type History struct {
	repo, all map[histKey][]histRun
	usdPerTok float64 // Claude's observed $ per fresh token
}

type histKey struct {
	role, kind string
	route      RouteKey
}

type histRun struct {
	tokens, usd, secs float64
}

// NewHistory indexes the routed runs of recs; root (may be "") is the repo
// whose runs come first.
func NewHistory(recs []Record, root string) *History {
	h := &History{repo: map[histKey][]histRun{}, all: map[histKey][]histRun{}}
	var claudeUSD, claudeTok float64
	inRepo := repoFilter(root)
	for _, r := range recs {
		if !routedStep(r) || r.Tokens == nil {
			continue
		}
		k := histKey{r.Role, r.Kind, RouteKey{r.Provider, r.Model, r.Effort}}
		run := histRun{tokens: float64(r.Tokens.Total()), usd: r.Tokens.CostUSD, secs: float64(r.DurationMS) / 1000}
		h.all[k] = append(h.all[k], run)
		if inRepo(r) {
			h.repo[k] = append(h.repo[k], run)
		}
		if r.Provider == event.Claude && r.Tokens.CostUSD > 0 {
			claudeUSD += r.Tokens.CostUSD
			claudeTok += float64(r.Tokens.Total())
		}
	}
	h.usdPerTok = defaultClaudeUSDPerToken
	if claudeTok > 0 {
		h.usdPerTok = claudeUSD / claudeTok
	}
	return h
}

// levelRuns returns a level's runs for a step: of its kind, plus runs logged
// before kinds were recorded.
func levelRuns(idx map[histKey][]histRun, role, kind string, route RouteKey) []histRun {
	out := append([]histRun(nil), idx[histKey{role, kind, route}]...)
	if kind != "" {
		out = append(out, idx[histKey{role, "", route}]...)
	}
	return out
}

// Estimate is what a step of this role and kind on this route will likely
// use. A nil History has no runs.
func (h *History) Estimate(role, kind string, route RouteKey, readOnly bool) Estimate {
	if h != nil {
		for _, lvl := range []struct {
			idx    map[histKey][]histRun
			source string
		}{{h.repo, SourceRepo}, {h.all, SourceAll}} {
			rs := levelRuns(lvl.idx, role, kind, route)
			if len(rs) < minEstimateSamples {
				continue
			}
			pick := func(f func(histRun) float64) Spread {
				vs := make([]float64, len(rs))
				for i, r := range rs {
					vs[i] = f(r)
				}
				return spread(vs)
			}
			return Estimate{
				Tokens:  pick(func(r histRun) float64 { return r.tokens }),
				Seconds: pick(func(r histRun) float64 { return r.secs }),
				USD:     pick(func(r histRun) float64 { return r.usd }),
				Samples: len(rs), Source: lvl.source,
			}
		}
	}
	tok, wall := float64(defaultWriteTokens), defaultWriteWall
	if readOnly {
		tok, wall = defaultReadTokens, defaultReadWall
	}
	e := Estimate{Tokens: wide(tok), Seconds: wide(wall.Seconds()), Source: SourceNone}
	if route.Provider == event.Claude {
		rate := defaultClaudeUSDPerToken
		if h != nil {
			rate = h.usdPerTok
		}
		e.USD = wide(tok * rate)
	}
	return e
}

// wide is a default's spread: half to twice the value.
func wide(v float64) Spread { return Spread{v / 2, v, v * 2} }

// spread is the 25th/50th/75th percentile of vs (linear interpolation).
func spread(vs []float64) Spread {
	sort.Float64s(vs)
	return Spread{percentile(vs, 0.25), percentile(vs, 0.5), percentile(vs, 0.75)}
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	pos := q * float64(len(sorted)-1)
	i := int(pos)
	if i+1 >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	return sorted[i] + (sorted[i+1]-sorted[i])*(pos-float64(i))
}
