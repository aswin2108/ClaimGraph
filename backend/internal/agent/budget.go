package agent

import (
	"sync"
	"time"
)

// Default spending guards (per process, in memory). Override with NewBudget.
//
// At gemini-3.8-flash list prices ($0.75 / $3.75 per 1M input / output tokens in 2026),
// a typical run (~20k input + ~4k output tokens) costs about 3 cents, and the default
// daily cap of 1,000,000 tokens is worth at most a few dollars even if every token were
// billed at the output rate.
const (
	DefaultMaxRunsPerHour   = 30
	DefaultDailyTokenBudget = 1_000_000
)

// Budget limits how much LLM work the whole process may do. When it is exhausted, agent
// runs keep working: they return the deterministic explanation without calling the model.
// Counters live in memory, so they reset when the server restarts.
type Budget struct {
	mu             sync.Mutex
	now            func() time.Time
	maxRunsPerHour int
	dailyTokens    int64
	runs           []time.Time // start times of LLM runs within the last hour
	day            string      // UTC date the token counter belongs to
	used           int64       // tokens used on that day
}

// BudgetStatus is a snapshot for display (it contains no secrets).
type BudgetStatus struct {
	RunsLastHour    int   `json:"llm_runs_last_hour"`
	MaxRunsPerHour  int   `json:"max_llm_runs_per_hour"`
	TokensToday     int64 `json:"llm_tokens_today"`
	DailyTokenLimit int64 `json:"daily_token_limit"`
}

// NewBudget returns a Budget. Non-positive limits fall back to the defaults, so the
// guard can never be switched off by accident.
func NewBudget(maxRunsPerHour int, dailyTokens int64) *Budget {
	if maxRunsPerHour <= 0 {
		maxRunsPerHour = DefaultMaxRunsPerHour
	}
	if dailyTokens <= 0 {
		dailyTokens = DefaultDailyTokenBudget
	}
	return &Budget{now: time.Now, maxRunsPerHour: maxRunsPerHour, dailyTokens: dailyTokens}
}

// Allow reports whether one more LLM run may start, and if so reserves its slot.
func (b *Budget) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.roll(now)
	if len(b.runs) >= b.maxRunsPerHour || b.used >= b.dailyTokens {
		return false
	}
	b.runs = append(b.runs, now)
	return true
}

// Commit records the tokens an LLM run actually consumed.
func (b *Budget) Commit(tokens int64) {
	if tokens <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll(b.now())
	b.used += tokens
}

// Status returns the current usage against the limits.
func (b *Budget) Status() BudgetStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll(b.now())
	return BudgetStatus{RunsLastHour: len(b.runs), MaxRunsPerHour: b.maxRunsPerHour, TokensToday: b.used, DailyTokenLimit: b.dailyTokens}
}

// roll drops expired entries and resets the daily counter at UTC midnight.
func (b *Budget) roll(now time.Time) {
	cutoff := now.Add(-time.Hour)
	keep := b.runs[:0]
	for _, t := range b.runs {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	b.runs = keep
	if d := now.UTC().Format("2006-01-02"); d != b.day {
		b.day, b.used = d, 0
	}
}
