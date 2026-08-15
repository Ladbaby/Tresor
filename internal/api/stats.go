package api

import (
	"net/http"
	"time"

	"tresor/internal/store"
)

// statsBucketHour is the bucket-key format used by the store layer.
const statsBucketHour = "2006-01-02T15:00:00Z"

// statsRangeParam maps a user-facing range preset to a (from, to, bucketSize) tuple.
// Returns false when the preset name is unknown.
func statsRangeParam(name string, now time.Time) (from, to time.Time, bucketSize string, ok bool) {
	// today: midnight to end-of-day (UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	endOfDay := today.Add(24 * time.Hour).Add(-time.Nanosecond)

	switch name {
	case "today":
		return today, endOfDay, "hour", true
	case "yesterday":
		y := today.Add(-24 * time.Hour)
		return y, y.Add(24*time.Hour).Add(-time.Nanosecond), "hour", true
	case "last_7_days":
		return today.Add(-7 * 24 * time.Hour), endOfDay, "day", true
	case "last_14_days":
		return today.Add(-14 * 24 * time.Hour), endOfDay, "day", true
	case "last_4_weeks":
		return today.Add(-28 * 24 * time.Hour), endOfDay, "day", true
	case "this_month":
		first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return first, endOfDay, "day", true
	case "last_month":
		firstOfThis := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		lastOfLast := firstOfThis.Add(-time.Nanosecond)
		firstOfLast := time.Date(lastOfLast.Year(), lastOfLast.Month(), 1, 0, 0, 0, 0, time.UTC)
		return firstOfLast, lastOfLast.Add(24*time.Hour).Add(-time.Nanosecond), "day", true
	case "this_year":
		first := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		return first, endOfDay, "day", true
	case "last_year":
		ly := now.Year() - 1
		start := time.Date(ly, 1, 1, 0, 0, 0, 0, time.UTC)
		end := time.Date(ly, 12, 31, 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
		return start, end, "day", true
	}
	return time.Time{}, time.Time{}, "", false
}

// parseStatsRange handles GET /api/stats query string parsing.
// Defaults to last_7_days when no range is provided.
func parseStatsRange(r *http.Request) (from, to time.Time, bucketSize string, err error) {
	now := time.Now().UTC()
	preset := r.URL.Query().Get("range")

	if preset == "custom" {
		fromStr := r.URL.Query().Get("from")
		toStr := r.URL.Query().Get("to")
		if fromStr == "" || toStr == "" {
			return time.Time{}, time.Time{}, "", &rangeError{msg: "from and to are required for custom range"}
		}
		from, perr := time.Parse("2006-01-02", fromStr)
		if perr != nil {
			return time.Time{}, time.Time{}, "", &rangeError{msg: "invalid from date (use YYYY-MM-DD)"}
		}
		from = from.UTC()
		to, perr = time.Parse("2006-01-02", toStr)
		if perr != nil {
			return time.Time{}, time.Time{}, "", &rangeError{msg: "invalid to date (use YYYY-MM-DD)"}
		}
		to = to.UTC().Add(24*time.Hour).Add(-time.Nanosecond)
		// hour buckets for short ranges, day buckets for everything else
		days := to.Sub(from).Hours() / 24
		if days <= 3 {
			return from, to, "hour", nil
		}
		return from, to, "day", nil
	}

	if preset == "" {
		preset = "last_7_days"
	}
	if f, t, bs, ok := statsRangeParam(preset, now); ok {
		return f, t, bs, nil
	}
	return time.Time{}, time.Time{}, "", &rangeError{msg: "unknown range preset: " + preset}
}

type rangeError struct{ msg string }

func (e *rangeError) Error() string { return e.msg }

// handleStats serves the aggregated token-usage statistics for the Dashboard tab.
// It is gated on the same auth middleware as the rest of the admin API.
func (r *Router) handleStats(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	from, to, bucketSize, err := parseStatsRange(req)
	if err != nil {
		if _, ok := err.(*rangeError); ok {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	query := store.StatsQuery{From: from, To: to}

	// capture_payloads is included so the frontend can show "N/A" for cache
	// hit rate when the feature is disabled, even if historical data exists.
	runtimeCfgMu.RLock()
	captureOn := runtimeCfg.CapturePayloads
	runtimeCfgMu.RUnlock()

	// Empty placeholder payload when the store is unavailable (e.g. tests).
	if r.store == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"capture_payloads": captureOn,
			"range":            map[string]string{"from": from.Format("2006-01-02"), "to": to.Format("2006-01-02")},
			"total": map[string]interface{}{
				"input_tokens":   int64(0),
				"output_tokens":  int64(0),
				"requests":       int64(0),
				"cache_hit_rate": nil,
			},
			"models":    []interface{}{},
			"providers": []interface{}{},
			"ips":       []interface{}{},
			"series":    []interface{}{},
		})
		return
	}

	totalIn, totalOut, totalReqs, cacheHitRate, err := r.store.TotalStats(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load totals: "+err.Error())
		return
	}

	models, err := r.store.AggregateStats(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load model stats: "+err.Error())
		return
	}

	providers, err := r.store.AggregateByProvider(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load provider stats: "+err.Error())
		return
	}

	ips, err := r.store.AggregateByIP(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load ip stats: "+err.Error())
		return
	}

	series, err := r.store.TimeSeries(query, bucketSize)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load time series: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"capture_payloads": captureOn,
		"range": map[string]string{
			"from": from.Format("2006-01-02"),
			"to":   to.Format("2006-01-02"),
		},
		"bucket_size": bucketSize,
		"total": map[string]interface{}{
			"input_tokens":   totalIn,
			"output_tokens":  totalOut,
			"requests":       totalReqs,
			"cache_hit_rate": cacheHitRate, // nil when no cache data
		},
		"models":    models,
		"providers": providers,
		"ips":       ips,
		"series":    series,
	})
}
