package policy

import (
	"sync"
	"time"
)

const (
	dayWindow          = 24 * time.Hour
	weekWindow         = 7 * 24 * time.Hour
	usageFlushInterval = 15 * time.Second
)

// usageLedger tracks per-key dollar usage with a daily window (UTC midnight
// reset) and a rolling 7-day weekly window. Usage is also broken down per alias.
//
// It is the in-memory source of truth; a background flusher periodically
// persists it to the state JSON (see Store.persistUsage). Reads for limit
// enforcement (Authenticate) and reporting (keys list) go through here.
type usageLedger struct {
	mu  sync.Mutex
	now func() time.Time
	// usage by key id; nil entry allowed when a key has no usage recorded yet.
	entries map[string]*UsageState
}

func newUsageLedger(now func() time.Time) *usageLedger {
	if now == nil {
		now = time.Now
	}
	return &usageLedger{now: now, entries: make(map[string]*UsageState)}
}

// loadFromState seeds the ledger from a loaded state file (restart recovery).
func (l *usageLedger) loadFromState(usage map[string]*UsageState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = make(map[string]*UsageState, len(usage))
	for id, st := range usage {
		if st == nil {
			continue
		}
		cp := *st
		l.entries[id] = &cp
	}
}

// snapshot returns a deep copy for persistence/reporting.
func (l *usageLedger) snapshot() map[string]*UsageState {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]*UsageState, len(l.entries))
	for id, st := range l.entries {
		if st == nil {
			continue
		}
		cp := *st
		out[id] = &cp
	}
	return out
}

func (l *usageLedger) entryLocked(id string) *UsageState {
	st := l.entries[id]
	if st == nil {
		st = &UsageState{ByAlias: make(map[string]UsageWindow)}
		l.entries[id] = st
	}
	if st.ByAlias == nil {
		st.ByAlias = make(map[string]UsageWindow)
	}
	return st
}

// ensureDailyWindow resets the daily window if we crossed UTC midnight since it
// last started. Caller must hold the mutex.
func (l *usageLedger) ensureDailyWindowLocked(st *UsageState, now time.Time) {
	startOfDay := now.UTC().Truncate(dayWindow)
	if st.Daily.WindowStart.IsZero() || !sameDay(st.Daily.WindowStart, startOfDay) {
		st.Daily = UsageWindow{WindowStart: startOfDay}
	}
}

func (l *usageLedger) ensureWeeklyWindowLocked(st *UsageState, now time.Time) {
	// Rolling window: if the recorded start is older than 7 days, slide it
	// forward so only the trailing 7 days count. We drop the accumulated total
	// and reset the window to now (conservative — losing usage that aged out
	// rather than recomputing partial slices; acceptable for an over-quota guard).
	if st.Weekly.WindowStart.IsZero() || now.Sub(st.Weekly.WindowStart) >= weekWindow {
		st.Weekly = UsageWindow{WindowStart: now.UTC()}
	}
}

// ensureAliasWindow applies the same window logic to a per-alias daily/weekly slice.
func (l *usageLedger) ensureAliasWindowLocked(w *UsageWindow, daily bool, now time.Time) {
	if daily {
		startOfDay := now.UTC().Truncate(dayWindow)
		if w.WindowStart.IsZero() || !sameDay(w.WindowStart, startOfDay) {
			*w = UsageWindow{WindowStart: startOfDay}
		}
		return
	}
	if w.WindowStart.IsZero() || now.Sub(w.WindowStart) >= weekWindow {
		*w = UsageWindow{WindowStart: now.UTC()}
	}
}

func sameDay(a, b time.Time) bool {
	a = a.UTC()
	b = b.UTC()
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

// RecordCost adds a dollar amount for a key+alias to the daily, weekly, and
// per-alias buckets, advancing windows as needed. It also accumulates the
// cache-specific counters (cache-read tokens, cache spend, non-cache input
// tokens) used for the cache hit-rate / spend report — these do NOT feed limit
// enforcement, only the Summary the UI reads.
//
// amount is the total dollar bill for the record; cacheCost is the portion of
// that bill attributable to cache-hit input tokens priced at the cache price
// (0 when no cache price was configured); cacheReadTokens is the cache-hit
// count for the record; inputTokens is the non-cache input-token count charged
// at the regular input price (the denominator partner for hit-rate).
func (l *usageLedger) RecordCost(id, alias string, amount, cacheCost float64, cacheReadTokens, inputTokens int64) {
	if amount <= 0 || id == "" {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.entryLocked(id)
	l.ensureDailyWindowLocked(st, now)
	l.ensureWeeklyWindowLocked(st, now)
	st.Daily.TotalUSD += amount
	st.Weekly.TotalUSD += amount
	if cacheReadTokens > 0 {
		st.Daily.CacheReadTokens += cacheReadTokens
		st.Weekly.CacheReadTokens += cacheReadTokens
	}
	if cacheCost > 0 {
		st.Daily.CacheCostUSD += cacheCost
		st.Weekly.CacheCostUSD += cacheCost
	}
	if inputTokens > 0 {
		st.Daily.InputTokens += inputTokens
		st.Weekly.InputTokens += inputTokens
	}

	aliasW := st.ByAlias[alias]
	l.ensureAliasWindowLocked(&aliasW, true, now)
	aliasW.TotalUSD += amount
	aliasW.CacheReadTokens += cacheReadTokens
	aliasW.CacheCostUSD += cacheCost
	aliasW.InputTokens += inputTokens
	st.ByAlias[alias] = aliasW
}

// UsageSummary is what the keys-list API reports for a key. The cache fields are
// reported for both the daily and weekly windows so the UI can show today's and
// the rolling-week's cache spend / hit-rate. CacheHitRate is not serialized
// here; the UI derives it as cacheRead / (cacheRead + input).
type UsageSummary struct {
	DailyUSD              float64   `json:"daily_usd"`
	WeeklyUSD             float64   `json:"weekly_usd"`
	DailyLimitUSD         float64   `json:"daily_limit_usd"`
	WeeklyLimitUSD        float64   `json:"weekly_limit_usd"`
	DailyResetAt          time.Time `json:"daily_reset_at,omitempty"`
	WeeklyResetAt         time.Time `json:"weekly_reset_at,omitempty"`
	DailyCacheCostUSD     float64   `json:"daily_cache_cost_usd,omitempty"`
	WeeklyCacheCostUSD    float64   `json:"weekly_cache_cost_usd,omitempty"`
	DailyCacheReadTokens  int64     `json:"daily_cache_read_tokens,omitempty"`
	WeeklyCacheReadTokens int64     `json:"weekly_cache_read_tokens,omitempty"`
	DailyInputTokens      int64     `json:"daily_input_tokens,omitempty"`
	WeeklyInputTokens     int64     `json:"weekly_input_tokens,omitempty"`
}

// Summary returns the current usage + limits for a key. Limits come from the
// KeyConfig; usage from the ledger. daily_reset_at = next UTC midnight;
// weekly_reset_at = window start + 7 days.
func (l *usageLedger) Summary(key KeyConfig) UsageSummary {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.entries[key.ID]
	summary := UsageSummary{
		DailyLimitUSD:  key.DailyLimitUSD,
		WeeklyLimitUSD: key.WeeklyLimitUSD,
		DailyResetAt:   now.UTC().Truncate(dayWindow).Add(dayWindow),
	}
	if st == nil {
		return summary
	}
	// Re-evaluate windows on read so a report never shows stale totals from a
	// window that already aged out.
	ensureSt := *st
	if ensureSt.ByAlias == nil {
		ensureSt.ByAlias = make(map[string]UsageWindow)
	}
	l.ensureDailyWindowLocked(&ensureSt, now)
	l.ensureWeeklyWindowLocked(&ensureSt, now)
	summary.DailyUSD = ensureSt.Daily.TotalUSD
	summary.WeeklyUSD = ensureSt.Weekly.TotalUSD
	summary.DailyCacheCostUSD = ensureSt.Daily.CacheCostUSD
	summary.WeeklyCacheCostUSD = ensureSt.Weekly.CacheCostUSD
	summary.DailyCacheReadTokens = ensureSt.Daily.CacheReadTokens
	summary.WeeklyCacheReadTokens = ensureSt.Weekly.CacheReadTokens
	summary.DailyInputTokens = ensureSt.Daily.InputTokens
	summary.WeeklyInputTokens = ensureSt.Weekly.InputTokens
	if !ensureSt.Weekly.WindowStart.IsZero() {
		summary.WeeklyResetAt = ensureSt.Weekly.WindowStart.Add(weekWindow)
	}
	return summary
}

// OverLimit reports whether a key is over its daily or weekly dollar limit.
// Returns the reason ("daily_exceeded"/"weekly_exceeded") and the offending
// summary when over; "" and zero summary otherwise.
func (l *usageLedger) OverLimit(key KeyConfig) (string, UsageSummary) {
	if key.DailyLimitUSD <= 0 && key.WeeklyLimitUSD <= 0 {
		return "", UsageSummary{}
	}
	s := l.Summary(key)
	if key.DailyLimitUSD > 0 && s.DailyUSD >= key.DailyLimitUSD {
		return "daily_exceeded", s
	}
	if key.WeeklyLimitUSD > 0 && s.WeeklyUSD >= key.WeeklyLimitUSD {
		return "weekly_exceeded", s
	}
	return "", UsageSummary{}
}

// resetUsage clears usage for a key (manual unlock) in memory only.
func (l *usageLedger) resetUsage(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, id)
}
