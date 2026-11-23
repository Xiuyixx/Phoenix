// Ignition targets: schedule, confirmation, failure backoff and circuit
// breaker, following Lamplighter internal/ignite (Schedule.DueAt, Observe,
// RecordFailure, Classify, CooldownWait) and internal/models (candidate
// ordering). The configured Ignition.Model always wins; model-list
// candidates are only used when it is empty.
package main

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// resetObservation is the last observed 5h reset of an ignition target.
type resetObservation struct {
	SeenUnix  float64  `json:"seen_epoch"`
	ResetUnix *float64 `json:"reset_epoch,omitempty"`
}

// targetState is the ignition schedule and failure-protection state of one
// credential. It is persisted in state.json.
type targetState struct {
	RollingReset         bool              `json:"rolling_reset,omitempty"`
	ResetObservation     *resetObservation `json:"reset_observation,omitempty"`
	LastSuccess          time.Time         `json:"last_success,omitempty"`
	LastAttempt          time.Time         `json:"last_attempt,omitempty"`
	LastModel            string            `json:"last_model,omitempty"`
	LastError            string            `json:"last_error,omitempty"`
	ConsecutiveFailures  int               `json:"consecutive_failures,omitempty"`
	RetryAt              time.Time         `json:"retry_at,omitempty"`
	CircuitUntil         time.Time         `json:"circuit_until,omitempty"`
	CircuitReason        string            `json:"circuit_reason,omitempty"`
	CircuitNotifiedUntil time.Time         `json:"circuit_notified_until,omitempty"`
	// lastCooldownFailureAt records when the last failure was a short CPA
	// cooldown, so a renewing cooldown still reaches failure protection.
	lastCooldownFailureAt time.Time `json:"-"`
}

const (
	// postSuccessHoldSeconds is the hold after a confirmed ignition during
	// which no further ignition is due (Lamplighter PostSuccessHoldSeconds).
	postSuccessHoldSeconds = 60
	// maxCooldownWait bounds the CPA cooldown that defers a retry; a longer
	// cooldown means the quota is still exhausted.
	maxCooldownWait = 10 * time.Minute
	// confirmDelays are the re-query delays after an ignition attempt while
	// waiting for a fixed future 5h reset to be observed.
)

var defaultConfirmDelays = []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second, 15 * time.Second}

// ErrNotConfirmed means the ignition request returned but no fixed 5h reset
// was observed afterwards.
var ErrNotConfirmed = errors.New("点火请求已返回，但未确认到新的固定 5h reset")

// ErrNoModel means CPA rejected every candidate model for the credential.
var ErrNoModel = errors.New("候选点火模型都不能用于该凭证")

// ignitionStatus classifies a target for the status payload.
func (t *targetState) ignitionStatus(now time.Time) string {
	if !t.CircuitUntil.IsZero() && now.Before(t.CircuitUntil) {
		return "circuit"
	}
	if !t.RetryAt.IsZero() && now.Before(t.RetryAt) {
		return "cooling"
	}
	return "ok"
}

// nextDailyStart returns the next start_hour after now, in loc.
func nextDailyStart(now time.Time, startHour int, loc *time.Location) time.Time {
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), startHour, 0, 0, 0, loc)
	if !local.Before(start) {
		start = start.AddDate(0, 0, 1)
	}
	return start.UTC()
}

// ignitionDueAt returns when the target should be ignited next. reset is
// the current 5h reset time, or zero when unknown. It mirrors Lamplighter
// ignite.Schedule.DueAt: outside the daily window [start_hour,
// end_hour + end_grace_minutes] the next daily start wins; a rolling
// (unstarted) 5h window ignites immediately; otherwise ignition is due at
// reset + grace_seconds when that falls inside the daily window.
func ignitionDueAt(cfg IgnitionConfig, ts *targetState, reset time.Time, now time.Time) time.Time {
	loc := time.Local
	localNow := now.In(loc)
	start := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), cfg.StartHour, 0, 0, 0, loc)
	end := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), cfg.EndHour, 0, 0, 0, loc).
		Add(time.Duration(cfg.EndGraceMinutes) * time.Minute)
	if !end.After(start) {
		end = end.AddDate(0, 0, 1)
	}
	if localNow.Before(start) {
		return start.UTC()
	}
	if localNow.After(end) {
		return nextDailyStart(now, cfg.StartHour, loc)
	}
	// Post-success hold and retry delay.
	if !ts.LastSuccess.IsZero() {
		if hold := ts.LastSuccess.Add(postSuccessHoldSeconds * time.Second); now.Before(hold) {
			if ts.RollingReset {
				return hold
			}
			// A fixed reset still wins over the hold only when it is due
			// later; otherwise the hold is the next due time.
			if reset.IsZero() || reset.After(now) {
				if !ts.RetryAt.IsZero() && ts.RetryAt.After(now) {
					return ts.RetryAt
				}
				return hold
			}
		}
	}
	if !ts.RetryAt.IsZero() && ts.RetryAt.After(now) {
		return ts.RetryAt
	}
	if ts.RollingReset {
		return now
	}
	if !reset.IsZero() && reset.After(now) {
		target := reset.Add(time.Duration(cfg.GraceSeconds) * time.Second)
		targetLocal := target.In(loc)
		sameDay := targetLocal.Year() == localNow.Year() && targetLocal.YearDay() == localNow.YearDay()
		if sameDay && !targetLocal.Before(start) && !targetLocal.After(end) {
			return target.UTC()
		}
		return nextDailyStart(now, cfg.StartHour, loc)
	}
	return now
}

// observeReset records one 5h reset observation and reports whether the
// window is a rolling placeholder: the reset stays about 5h ahead and moves
// forward with every observation. A started window has a fixed reset time.
// Two observations tell them apart (Lamplighter ignite.Observe).
func observeReset(ts *targetState, reset, now time.Time, passive bool) bool {
	rolling := false
	var resetEpoch *float64
	if !reset.IsZero() {
		value := float64(reset.UnixNano()) / 1e9
		resetEpoch = &value
		prev := ts.ResetObservation
		offset := reset.Sub(now)
		if !passive && prev != nil && prev.ResetUnix != nil && absDuration(offset-5*time.Hour) <= 120*time.Second {
			elapsed := time.Duration((float64(now.UnixNano())/1e9 - prev.SeenUnix) * float64(time.Second))
			shift := time.Duration((value - *prev.ResetUnix) * float64(time.Second))
			if elapsed >= 2*time.Second {
				tolerance := time.Duration(float64(elapsed) * 0.02)
				if tolerance < 3*time.Second {
					tolerance = 3 * time.Second
				}
				if tolerance > 10*time.Second {
					tolerance = 10 * time.Second
				}
				rolling = absDuration(shift-elapsed) <= tolerance
			}
		}
	}
	ts.RollingReset = rolling
	if !rolling && !reset.IsZero() && reset.After(now) {
		clearTargetFailures(ts)
	}
	ts.ResetObservation = &resetObservation{SeenUnix: float64(now.UnixNano()) / 1e9, ResetUnix: resetEpoch}
	return rolling
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// clearTargetFailures resets the failure counter and closes the circuit.
func clearTargetFailures(ts *targetState) {
	ts.ConsecutiveFailures = 0
	ts.RetryAt = time.Time{}
	ts.CircuitUntil = time.Time{}
	ts.CircuitReason = ""
	ts.CircuitNotifiedUntil = time.Time{}
}

// recordTargetSuccess stores a confirmed ignition.
func recordTargetSuccess(ts *targetState, model string, now time.Time) {
	ts.LastSuccess = now
	ts.LastModel = model
	ts.LastError = ""
	clearTargetFailures(ts)
}

var hardIgniteStatuses = map[int]bool{400: true, 401: true, 403: true, 404: true, 409: true, 422: true, 429: true}

var resetSecondsPattern = regexp.MustCompile(`"reset_seconds"\s*:\s*(\d+)`)

// cooldownWait returns how long CPA keeps the credential in its local
// cooldown, from the reset_seconds of a model_cooldown error. Only waits of
// at most 10 minutes are honoured; longer ones mean the quota is still
// exhausted (Lamplighter ignite.CooldownWait).
func cooldownWait(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	text := err.Error()
	if !strings.Contains(text, "model_cooldown") && !strings.Contains(text, "are cooling down") {
		return 0, false
	}
	match := resetSecondsPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	seconds, convErr := strconv.Atoi(match[1])
	if convErr != nil || seconds <= 0 {
		return 0, false
	}
	wait := time.Duration(seconds) * time.Second
	return wait, wait <= maxCooldownWait
}

// classifyIgniteError sorts an ignition error into "hard", "transient" or
// "unsupported". Hard errors (400/401/403/404/409/422/429, unconfirmed, no
// model) and exhausting the transient budget open the circuit breaker.
func classifyIgniteError(err error, status int) string {
	if err == nil {
		return "transient"
	}
	if errors.Is(err, ErrNotConfirmed) || errors.Is(err, ErrNoModel) {
		return "hard"
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "auth_not_found") || strings.Contains(text, "unknown provider for model") {
		return "unsupported"
	}
	if strings.Contains(text, "are cooling down") || strings.Contains(text, "auth_unavailable") {
		return "transient"
	}
	if hardIgniteStatuses[status] {
		return "hard"
	}
	for _, marker := range []string{
		"http 400", "http 401", "http 403", "http 404", "http 409", "http 422", "http 429",
		"status 400", "status 401", "status 403", "status 404", "status 409", "status 422", "status 429",
		"not_found", "not found", "unsupported", "invalid model", "credential is unavailable",
	} {
		if strings.Contains(text, marker) {
			return "hard"
		}
	}
	return "transient"
}

// modelRejected reports whether the provider refused the model itself
// (HTTP 400 or 404). Authentication errors and rate limits concern the
// account and are not model rejections.
func modelRejected(err error, status int) bool {
	if classifyIgniteError(err, status) != "hard" || errors.Is(err, ErrNotConfirmed) || errors.Is(err, ErrNoModel) {
		return false
	}
	if status != 0 {
		return status == 400 || status == 404
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"http 400", "http 404", "status 400", "status 404", "not_found", "not found", "invalid model"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// failureOutcome describes what recordTargetFailure decided.
type failureOutcome struct {
	CircuitOpened bool
	NotifyCircuit bool
	Until         time.Time
	RetryIn       time.Duration
	Cooldown      bool
}

// recordTargetFailure updates the failure state. A short CPA cooldown is
// waited out instead (retry when it ends plus grace, failure not counted);
// hard errors and the last allowed transient error open the circuit until
// the next daily start; other transient failures retry with
// failure_retry_seconds x backoff 3 (Lamplighter ignite.RecordFailure).
func recordTargetFailure(ts *targetState, cfg IgnitionConfig, now time.Time, err error, status int) failureOutcome {
	if wait, ok := cooldownWait(err); ok && !recentCooldownFailure(ts, now) {
		delay := wait + time.Duration(cfg.GraceSeconds)*time.Second
		ts.LastError = truncateRunes(err.Error(), 500)
		ts.LastAttempt = now
		ts.lastCooldownFailureAt = now
		ts.RetryAt = now.Add(delay)
		return failureOutcome{RetryIn: delay, Cooldown: true}
	}
	ts.ConsecutiveFailures++
	ts.LastError = truncateRunes(err.Error(), 500)
	ts.LastAttempt = now
	maxFailures := cfg.MaxTransientFailures
	if maxFailures <= 0 {
		maxFailures = 3
	}
	if classifyIgniteError(err, status) == "hard" || ts.ConsecutiveFailures >= maxFailures {
		until := nextDailyStart(now, cfg.StartHour, time.Local)
		ts.CircuitUntil = until
		ts.RetryAt = until
		ts.CircuitReason = truncateRunes(err.Error(), 500)
		notify := ts.CircuitNotifiedUntil.IsZero() || !ts.CircuitNotifiedUntil.Equal(until)
		ts.CircuitNotifiedUntil = until
		return failureOutcome{CircuitOpened: true, NotifyCircuit: notify, Until: until}
	}
	delay := time.Duration(cfg.FailureRetrySeconds) * time.Second
	if delay <= 0 {
		delay = 300 * time.Second
	}
	for i := 1; i < ts.ConsecutiveFailures; i++ {
		delay *= 3
	}
	ts.RetryAt = now.Add(delay)
	return failureOutcome{RetryIn: delay}
}

func recentCooldownFailure(ts *targetState, now time.Time) bool {
	if ts.lastCooldownFailureAt.IsZero() || now.Sub(ts.lastCooldownFailureAt) > 2*maxCooldownWait {
		return false
	}
	_, ok := cooldownWait(errors.New(ts.LastError))
	return ok
}

func truncateRunes(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

// --- model list / candidate selection (Lamplighter internal/models) ---

var modelDigits = regexp.MustCompile(`\d+`)

func modelRecencyKey(model string) []int {
	parts := modelDigits.FindAllString(model, -1)
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, _ := strconv.Atoi(part)
		out = append(out, n)
	}
	return out
}

func modelNewerFirst(a, b string) bool {
	ka, kb := modelRecencyKey(a), modelRecencyKey(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i] != kb[i] {
			return ka[i] > kb[i]
		}
	}
	if len(ka) != len(kb) {
		return len(ka) > len(kb)
	}
	return a < b
}

// pickIgnitionCandidates orders available models the way Lamplighter's
// codexCandidates does: newest non-image luna first, then gpt-* excluding
// oss/image, at most 6 candidates.
func pickIgnitionCandidates(available []string) []string {
	noImage := func(m string) bool { return !strings.Contains(m, "image") }
	var luna, gpt []string
	for _, m := range available {
		if strings.Contains(m, "luna") && noImage(m) {
			luna = append(luna, m)
		}
	}
	sort.Slice(luna, func(i, j int) bool { return modelNewerFirst(luna[i], luna[j]) })
	for _, m := range available {
		if strings.HasPrefix(m, "gpt-") && !strings.Contains(m, "oss") && noImage(m) {
			gpt = append(gpt, m)
		}
	}
	sort.Slice(gpt, func(i, j int) bool { return modelNewerFirst(gpt[i], gpt[j]) })
	out := append(luna, gpt...)
	seen := map[string]bool{}
	uniq := out[:0]
	for _, m := range out {
		if !seen[m] {
			seen[m] = true
			uniq = append(uniq, m)
		}
	}
	if len(uniq) > 6 {
		uniq = uniq[:6]
	}
	return uniq
}

// ignitionCandidateModels orders the models one ignition attempt tries: the
// configured model first, then CPA's listed candidates for the provider
// (deduplicated, at most four of them), then gpt-5-codex as a last resort
// for Codex when the list offers nothing usable.
func ignitionCandidateModels(configured string, listed []string, provider string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		out = append(out, m)
	}
	add(configured)
	for _, m := range listed {
		if len(out) >= 5 {
			break
		}
		add(m)
	}
	if provider == "codex" {
		add("gpt-5-codex")
	}
	return out
}

// isFixedReset reports whether reset is a fixed future reset (not zero, not
// in the past, and not a rolling placeholder that moves with time).
func isFixedReset(reset time.Time, now time.Time) bool {
	return !reset.IsZero() && reset.After(now)
}
