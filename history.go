// History persistence: quota samples and events are appended as JSONL
// records, one file per UTC day under <data dir>/history, following
// Lamplighter's store.History layout. When no data dir can be resolved the
// records live in memory only (see persist.go); nothing is written to a
// hardcoded host path.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// historyRecord is one line of a history file: a quota sample ("s") or an
// event ("e"). Field names mirror Lamplighter's store.Record so the files
// stay comparable.
type historyRecord struct {
	Kind      string            `json:"k"`           // "s" sample, "e" event
	Time      int64             `json:"t"`           // Unix seconds
	Group     string            `json:"g,omitempty"` // auth_index
	Window    string            `json:"w,omitempty"` // "5h" | "7d", samples only
	Remaining float64           `json:"r,omitempty"` // remaining percent, samples only
	Reset     int64             `json:"x,omitempty"` // reset Unix seconds, samples only
	Source    string            `json:"s,omitempty"` // "active" | "passive", samples only
	Event     string            `json:"e,omitempty"` // event type, events only
	Level     string            `json:"v,omitempty"` // "info" | "ok" | "error", events only
	Message   string            `json:"m,omitempty"` // event text, events only
	Params    map[string]string `json:"p,omitempty"` // event params, events only
}

// historyFileFor returns the JSONL path for the UTC day of t.
func historyFileFor(dir string, t time.Time) string {
	return filepath.Join(dir, t.UTC().Format("2006-01-02")+".jsonl")
}

// appendHistory writes records to the file of their day. dir == "" means
// memory-only mode; the caller keeps them in rt.histMem instead.
func appendHistory(dir string, records ...historyRecord) error {
	if dir == "" || len(records) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	byFile := map[string][]historyRecord{}
	for _, r := range records {
		byFile[historyFileFor(dir, time.Unix(r.Time, 0))] = append(byFile[historyFileFor(dir, time.Unix(r.Time, 0))], r)
	}
	for path, items := range byFile {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(f)
		for _, item := range items {
			raw, err := json.Marshal(item)
			if err != nil {
				continue
			}
			w.Write(raw)
			w.WriteByte('\n')
		}
		flushErr := w.Flush()
		closeErr := f.Close()
		if flushErr != nil {
			return flushErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// scanHistory reads the records between from and to (inclusive), oldest
// first, from files on disk plus in-memory records.
func scanHistory(dir string, mem []historyRecord, from, to time.Time) []historyRecord {
	var out []historyRecord
	if dir != "" {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
					continue
				}
				day, err := time.Parse("2006-01-02", strings.TrimSuffix(entry.Name(), ".jsonl"))
				if err != nil || day.After(to) || day.Add(24*time.Hour).Before(from) {
					continue
				}
				f, err := os.Open(filepath.Join(dir, entry.Name()))
				if err != nil {
					continue
				}
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 64*1024), 1024*1024)
				for sc.Scan() {
					var rec historyRecord
					if json.Unmarshal(sc.Bytes(), &rec) != nil {
						continue
					}
					if rec.Time < from.Unix() || rec.Time > to.Unix() {
						continue
					}
					out = append(out, rec)
				}
				f.Close()
			}
		}
	}
	for _, rec := range mem {
		if rec.Time >= from.Unix() && rec.Time <= to.Unix() {
			out = append(out, rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

// pruneHistory deletes history files older than retentionDays (counted in
// whole UTC days) and drops in-memory records older than the cutoff. It
// returns the pruned in-memory slice.
func pruneHistory(dir string, mem []historyRecord, retentionDays int, now time.Time) []historyRecord {
	if retentionDays < 1 {
		retentionDays = 40
	}
	cutoff := now.AddDate(0, 0, -retentionDays)
	if dir != "" {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
					continue
				}
				day, err := time.Parse("2006-01-02", strings.TrimSuffix(entry.Name(), ".jsonl"))
				if err == nil && day.Add(24*time.Hour).Before(cutoff) {
					_ = os.Remove(filepath.Join(dir, entry.Name()))
				}
			}
		}
	}
	kept := mem[:0]
	for _, rec := range mem {
		if rec.Time >= cutoff.Unix() {
			kept = append(kept, rec)
		}
	}
	return kept
}

// loadRecentEvents reloads event records from the last days days of history
// files, newest first, capped at maxEvents. Used at startup so the panel's
// event list survives a restart.
func loadRecentEvents(dir string, days int, maxEvents int, now time.Time) []panelEvent {
	if dir == "" {
		return nil
	}
	records := scanHistory(dir, nil, now.AddDate(0, 0, -days), now)
	var events []panelEvent
	for i := len(records) - 1; i >= 0; i-- {
		rec := records[i]
		if rec.Kind != "e" {
			continue
		}
		events = append(events, panelEvent{Time: time.Unix(rec.Time, 0), Kind: rec.Level, Message: rec.Message, AuthIndex: rec.Group, Window: rec.Window, Event: rec.Event, Params: rec.Params})
		if len(events) >= maxEvents {
			break
		}
	}
	return events
}

// historyRanges maps a range name to its span. 5h-window ranges are in
// hours, 7d-window ranges in days; "1mo" reaches back to the same date of
// the previous month (Lamplighter RangeSpan).
var historyRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"3h":  3 * time.Hour,
	"6h":  6 * time.Hour,
	"12h": 12 * time.Hour,
	"24h": 24 * time.Hour,
	"26h": 26 * time.Hour,
	"1d":  24 * time.Hour,
	"4d":  4 * 24 * time.Hour,
	"8d":  8 * 24 * time.Hour,
	"15d": 15 * 24 * time.Hour,
	"36d": 36 * 24 * time.Hour,
}

// historyRangeSpan returns the span of a named range, defaulting per window
// kind (6h for 5h windows, 8d for 7d windows).
func historyRangeSpan(name, window string, now time.Time) time.Duration {
	if span, ok := historyRanges[name]; ok {
		return span
	}
	if name == "1mo" {
		local := now
		year, month, day := local.Date()
		if last := time.Date(year, month, 0, 0, 0, 0, 0, local.Location()).Day(); day > last {
			day = last
		}
		from := time.Date(year, month-1, day, local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), local.Location())
		return now.Sub(from)
	}
	if window == windowSevenDay {
		return 8 * 24 * time.Hour
	}
	return 6 * time.Hour
}

// historyBucket returns the bucket length in seconds for a span, following
// the audit contract: spans up to 2 days keep (near-)raw samples at a 60s
// nominal bucket, then 600s over 2d, 900s over 4d, 1800s over 8d and 3600s
// over 15d. A 60s bucket loses nothing in practice because the active poll
// interval is at least 60s.
func historyBucket(span time.Duration) int64 {
	const day = 24 * time.Hour
	switch {
	case span > 15*day:
		return 3600
	case span > 8*day:
		return 1800
	case span > 4*day:
		return 900
	case span > 2*day:
		return 600
	}
	return 60
}

// bucketSamples reduces samples to the last sample per bucket (bucket <= 0
// keeps every sample) and prepends the last sample before from, so each
// series reaches the left edge of the range.
func bucketSamples(samples []historyRecord, from int64, bucket int64) [][3]any {
	if bucket <= 0 {
		bucket = 1
	}
	kept := map[int64]historyRecord{}
	var order []int64
	var before *historyRecord
	for i := range samples {
		rec := samples[i]
		if rec.Time < from {
			c := rec
			before = &c
			continue
		}
		b := rec.Time / bucket
		if _, ok := kept[b]; !ok {
			order = append(order, b)
		}
		kept[b] = rec
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	var points [][3]any
	if before != nil {
		points = append(points, [3]any{before.Time, before.Remaining, before.Source})
	}
	for _, b := range order {
		rec := kept[b]
		points = append(points, [3]any{rec.Time, rec.Remaining, rec.Source})
	}
	return points
}

// resetMarkers returns [reset_time_unix, remaining_after_reset] pairs:
// when a sample reports a materially different reset time (>60s) from the
// previous sample of the same series, a window reset happened at that new
// reset time, and the sample's remaining is the remaining after the reset.
// The frontend derives pace segments as [reset - period, reset] from these.
func resetMarkers(samples []historyRecord, from int64) [][2]any {
	var out [][2]any
	var prevReset int64
	seen := false
	for i := range samples {
		rec := samples[i]
		if !seen || (rec.Reset == 0) != (prevReset == 0) || absInt64(rec.Reset-prevReset) > 60 {
			if rec.Time >= from && rec.Reset != 0 {
				out = append(out, [2]any{rec.Reset, rec.Remaining})
			}
		}
		prevReset = rec.Reset
		seen = true
	}
	return out
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
