// Package limits detects usage-limit messages and tracks which provider is
// currently at its subscription limit.
package limits

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sparkz400/switchyard/internal/event"
)

// Detector matches usage-limit text.
type Detector struct{ res []*regexp.Regexp }

// NewDetector compiles case-insensitive patterns; invalid ones are skipped
// (config validation reports them).
func NewDetector(patterns []string) *Detector {
	d := &Detector{}
	for _, p := range patterns {
		if re, err := regexp.Compile("(?i)" + p); err == nil {
			d.res = append(d.res, re)
		}
	}
	return d
}

// Match reports whether text looks like a usage-limit message. Only call it
// on error text: a model talking about "quota" in a normal reply is not a
// limit hit.
func (d *Detector) Match(text string) bool {
	if d == nil || text == "" {
		return false
	}
	for _, re := range d.res {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

var (
	reTryAtClock = regexp.MustCompile(`(?i)(?:try again|resets?)\s+(?:at|in)?\s*(\d{1,2}):(\d{2})\s*(am|pm)?`)
	reTryIn      = regexp.MustCompile(`(?i)(?:try again|resets?)\s+in\s+(?:(\d+)\s*h(?:ours?)?)?\s*(?:(\d+)\s*m(?:in(?:utes?)?)?)?`)
	reEpoch      = regexp.MustCompile(`\|(\d{10})\b`) // Claude: "Claude AI usage limit reached|1791003600"
)

// ParseReset tries to find when a limit resets in a limit message. It
// understands "try again at 3:05 PM", "try again in 2h 10m" and Claude's
// "...limit reached|<unix seconds>". ok is false when nothing was found.
func ParseReset(text string, now time.Time) (time.Time, bool) {
	if m := reEpoch.FindStringSubmatch(text); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return time.Unix(sec, 0), true
		}
	}
	if m := reTryIn.FindStringSubmatch(text); m != nil && (m[1] != "" || m[2] != "") {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		return now.Add(time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute), true
	}
	if m := reTryAtClock.FindStringSubmatch(text); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		switch strings.ToLower(m[3]) {
		case "pm":
			if h < 12 {
				h += 12
			}
		case "am":
			if h == 12 {
				h = 0
			}
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return t, true
	}
	return time.Time{}, false
}

// ProviderState is a snapshot of one provider.
type ProviderState struct {
	Provider     string
	LimitedUntil time.Time // zero when available
	Tokens       event.TokenUsage
	Calls        int
	LimitHits    int
	Quota        *event.QuotaInfo
}

// Limited reports whether the provider is at its limit at time now.
func (s ProviderState) Limited(now time.Time) bool { return now.Before(s.LimitedUntil) }

// Tracker holds per-provider state. It is safe for concurrent use.
type Tracker struct {
	mu    sync.Mutex
	state map[string]*ProviderState
	now   func() time.Time
}

// NewTracker creates a tracker for both providers.
func NewTracker() *Tracker {
	t := &Tracker{state: map[string]*ProviderState{}, now: time.Now}
	for _, p := range event.Providers {
		t.state[p] = &ProviderState{Provider: p}
	}
	return t
}

// SetClock overrides the clock (tests).
func (t *Tracker) SetClock(now func() time.Time) { t.now = now }

// Limited reports whether a provider is currently at its limit.
func (t *Tracker) Limited(p string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.state[p]
	return ok && s.Limited(t.now())
}

// MarkLimited marks a provider as limited until the given time.
func (t *Tracker) MarkLimited(p string, until time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.state[p]; ok {
		s.LimitedUntil = until
		s.LimitHits++
	}
}

// Clear marks a provider available again.
func (t *Tracker) Clear(p string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.state[p]; ok {
		s.LimitedUntil = time.Time{}
	}
}

// AddUsage records tokens spent on a provider.
func (t *Tracker) AddUsage(p string, u event.TokenUsage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.state[p]; ok {
		s.Tokens = s.Tokens.Add(u)
		s.Calls++
	}
}

// SetQuota records provider-reported quota utilization.
func (t *Tracker) SetQuota(p string, q event.QuotaInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.state[p]; ok {
		s.Quota = &q
	}
}

// Snapshot returns a copy of a provider's state.
func (t *Tracker) Snapshot(p string) ProviderState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.state[p]; ok {
		c := *s
		if s.Quota != nil {
			q := *s.Quota
			c.Quota = &q
		}
		return c
	}
	return ProviderState{Provider: p}
}

// Share returns the fraction of all session tokens spent on p, used by the
// "auto" prefer mode.
func (t *Tracker) Share(p string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state[p] == nil {
		return 0
	}
	var total int64
	for _, s := range t.state {
		total += s.Tokens.Total()
	}
	if total == 0 {
		return 0
	}
	return float64(t.state[p].Tokens.Total()) / float64(total)
}
