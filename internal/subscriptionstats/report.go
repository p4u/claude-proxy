// Package subscriptionstats reports recorded proxy traffic and conservative,
// workload-conditioned equivalents of observed subscription quota consumption.
// It never treats tokens as a published allowance or changes routing state.
package subscriptionstats

// Tokens separates the four recorded token categories. Input excludes cache
// reads (including for custom OpenAI hosts whose stored input includes them).
// Total is their sum, except in Group's marginal quantile summaries: the
// quantile of totals need not equal the sum of each category's quantile.
type Tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	CacheRead     int64 `json:"cache_read"`
	Total         int64 `json:"total"`
}

// Report contains measured traffic in [From, To). All timestamps are Unix
// seconds. Current account windows are as of AsOf, independently of that range.
type Report struct {
	From            int64            `json:"from"`
	To              int64            `json:"to"`
	AsOf            int64            `json:"as_of"`
	Window          string           `json:"window"`
	Requests        int64            `json:"requests"`
	Tokens          Tokens           `json:"tokens"`
	Groups          []Group          `json:"groups"`
	Accounts        []Account        `json:"accounts"`
	Daily           []Daily          `json:"daily"`
	CapacityHistory []CapacitySample `json:"capacity_history"`
	Models          []Model          `json:"models"`
	Notes           []string         `json:"notes"`
	Truncated       bool             `json:"truncated"`
}

// Group is a cohort under the credential's current provider, plan and known
// rate-limit tier. Gateway groups have no per-account token attribution.
type Group struct {
	Key             string  `json:"key"`
	Provider        string  `json:"provider"`
	ProviderName    string  `json:"provider_name"`
	Plan            string  `json:"plan"`
	Tier            string  `json:"tier"`
	Label           string  `json:"label"`
	Attribution     string  `json:"attribution"`
	Credentials     int     `json:"credentials"`
	Requests        int64   `json:"requests"`
	Tokens          Tokens  `json:"tokens"`
	Estimate        *Tokens `json:"estimate"`
	EstimateSamples int     `json:"estimate_samples"`
	EstimateLow     *Tokens `json:"estimate_low"`
	EstimateHigh    *Tokens `json:"estimate_high"`
}

// Account identifies a stored credential or a retained unknown/deleted ID, not
// necessarily an upstream account. Attribution distinguishes that difference.
type Account struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Provider    string  `json:"provider"`
	Plan        string  `json:"plan"`
	Tier        string  `json:"tier"`
	GroupKey    string  `json:"group_key"`
	Attribution string  `json:"attribution"`
	Requests    int64   `json:"requests"`
	Tokens      Tokens  `json:"tokens"`
	Current     *Window `json:"current"`
}

// Window aligns recorded traffic from its nominal Start through ObservedAt
// (inclusive) with UsedPct. Estimate instead uses a matched observation delta;
// it never divides these whole-window tokens by UsedPct. Reason explains
// unavailable or incomplete evidence; an empty reason means no detected issue.
type Window struct {
	Start      int64   `json:"start"`
	ResetAt    int64   `json:"reset_at"`
	ObservedAt int64   `json:"observed_at"`
	UsedPct    float64 `json:"used_pct"`
	Requests   int64   `json:"requests"`
	Tokens     Tokens  `json:"tokens"`
	Estimate   *Tokens `json:"estimate"`
	DeltaPct   float64 `json:"delta_pct"`
	Samples    int     `json:"samples"`
	Reason     string  `json:"reason"`
}

// CapacitySample is one contiguous, usable interval within an account/quota
// cycle. Tokens cover (Start, End]; Estimate scales each category by
// 100/DeltaPct for that observed model/cache mix, not a published token quota.
type CapacitySample struct {
	GroupKey     string  `json:"group_key"`
	CredentialID string  `json:"credential_id"`
	Start        int64   `json:"start"`
	End          int64   `json:"end"`
	ResetAt      int64   `json:"reset_at"`
	ObservedAt   int64   `json:"observed_at"`
	Tokens       Tokens  `json:"tokens"`
	Estimate     Tokens  `json:"estimate"`
	DeltaPct     float64 `json:"delta_pct"`
	Samples      int     `json:"samples"`
}

// Daily is recorded traffic grouped by UTC day and current credential cohort.
type Daily struct {
	TS       int64  `json:"ts"`
	GroupKey string `json:"group_key"`
	Requests int64  `json:"requests"`
	Tokens   Tokens `json:"tokens"`
}

// Model is the recorded response/request model, without provider inference.
// An empty Model means that the request log did not record a model name.
type Model struct {
	GroupKey string `json:"group_key"`
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   Tokens `json:"tokens"`
}
