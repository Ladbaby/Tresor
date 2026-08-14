package store

import (
	"fmt"
	"time"
)

// UsageStatsRow is one row of the per-(downstream, model) aggregate.
type UsageStatsRow struct {
	DownstreamID  string `json:"downstream_id"`
	Model         string `json:"model"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheCreation int64  `json:"cache_creation_tokens"`
	CacheRead     int64  `json:"cache_read_tokens"`
	RequestCount  int64  `json:"request_count"`
	CacheHitCount int64  `json:"cache_hit_count"`
}

// TimeSeriesPoint is one bucket in the time-series response.
type TimeSeriesPoint struct {
	Bucket       string `json:"bucket"` // ISO8601 hour ("YYYY-MM-DDTHH:00:00Z") or date ("YYYY-MM-DD")
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	RequestCount int64  `json:"request_count"`
}

// StatsQuery holds query parameters for the stats API.
type StatsQuery struct {
	From, To time.Time
}

// bucketKey returns the canonical ISO8601-hour bucket key for a timestamp.
func bucketKey(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:00:00Z")
}

// RecordUsageStats upserts one request's usage into the hourly bucket.
// Only call this for the final attempt of a request (not retry attempts).
func (s *Store) RecordUsageStats(bucket, downstreamID, model string,
	input, output, cacheCreation, cacheRead int64) error {
	if bucket == "" {
		bucket = bucketKey(time.Now())
	}
	cacheHitIncrement := int64(0)
	if cacheRead > 0 {
		cacheHitIncrement = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO usage_stats
		    (bucket, downstream_id, model, input_tokens, output_tokens,
		     cache_creation, cache_read, request_count, cache_hit_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(bucket, downstream_id, model) DO UPDATE SET
		    input_tokens    = input_tokens    + excluded.input_tokens,
		    output_tokens   = output_tokens   + excluded.output_tokens,
		    cache_creation  = cache_creation  + excluded.cache_creation,
		    cache_read      = cache_read      + excluded.cache_read,
		    request_count   = request_count   + 1,
		    cache_hit_count = cache_hit_count + excluded.cache_hit_count
	`, bucket, downstreamID, model, input, output, cacheCreation, cacheRead, cacheHitIncrement)
	if err != nil {
		return fmt.Errorf("record usage stats: %w", err)
	}
	return nil
}

// AggregateStats returns per-(downstream, model) aggregates for the given time range,
// ordered by total tokens (input + output) descending.
func (s *Store) AggregateStats(q StatsQuery) ([]UsageStatsRow, error) {
	fromKey := bucketKey(q.From)
	toKey := bucketKey(q.To)
	rows, err := s.db.Query(`
		SELECT downstream_id, model,
		       COALESCE(SUM(input_tokens), 0),    COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0),
		       COALESCE(SUM(cache_read), 0),
		       COALESCE(SUM(request_count), 0),
		       COALESCE(SUM(cache_hit_count), 0)
		FROM usage_stats
		WHERE bucket >= ? AND bucket <= ?
		GROUP BY downstream_id, model
		ORDER BY (SUM(input_tokens) + SUM(output_tokens)) DESC
	`, fromKey, toKey)
	if err != nil {
		return nil, fmt.Errorf("aggregate stats: %w", err)
	}
	defer rows.Close()

	result := []UsageStatsRow{}
	for rows.Next() {
		var r UsageStatsRow
		if err := rows.Scan(&r.DownstreamID, &r.Model,
			&r.InputTokens, &r.OutputTokens,
			&r.CacheCreation, &r.CacheRead,
			&r.RequestCount, &r.CacheHitCount); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// TimeSeries returns bucketed totals for the given range.
// bucketSize is "hour" or "day". For "day", the bucket label is "YYYY-MM-DD".
// For "hour", the bucket label is the canonical ISO8601 hour key.
func (s *Store) TimeSeries(q StatsQuery, bucketSize string) ([]TimeSeriesPoint, error) {
	fromKey := bucketKey(q.From)
	toKey := bucketKey(q.To)

	var groupExpr string
	switch bucketSize {
	case "day":
		groupExpr = `DATE(bucket)`
	default:
		groupExpr = `bucket`
	}

	query := `
		SELECT ` + groupExpr + ` AS ts,
		       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(request_count), 0)
		FROM usage_stats
		WHERE bucket >= ? AND bucket <= ?
		GROUP BY ts
		ORDER BY ts ASC
	`
	rows, err := s.db.Query(query, fromKey, toKey)
	if err != nil {
		return nil, fmt.Errorf("time series: %w", err)
	}
	defer rows.Close()

	result := []TimeSeriesPoint{}
	for rows.Next() {
		var p TimeSeriesPoint
		if err := rows.Scan(&p.Bucket, &p.InputTokens, &p.OutputTokens, &p.RequestCount); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// TotalStats returns the grand totals + computed cache hit rate for the time range.
// cacheHitRate is nil when no cache data exists in the range (frontend shows "N/A"
// or "—" depending on whether capture_payloads is enabled).
func (s *Store) TotalStats(q StatsQuery) (input, output, requests int64, cacheHitRate *float64, err error) {
	fromKey := bucketKey(q.From)
	toKey := bucketKey(q.To)
	var totalCacheRead int64

	row := s.db.QueryRow(`
		SELECT COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(request_count), 0),
		       COALESCE(SUM(cache_read), 0),
		       COALESCE(SUM(cache_hit_count), 0)
		FROM usage_stats
		WHERE bucket >= ? AND bucket <= ?
	`, fromKey, toKey)
	if err := row.Scan(&input, &output, &requests, &totalCacheRead, new(int64)); err != nil {
		return 0, 0, 0, nil, fmt.Errorf("total stats: %w", err)
	}

	if totalCacheRead > 0 {
		// rate = cache_read / (input_tokens + cache_read)
		// This matches the semantics of UsageBlock.CacheHitRate:
		// input_tokens is the non-cached portion, cache_read fills the rest.
		denom := input + totalCacheRead
		if denom > 0 {
			rate := float64(totalCacheRead) / float64(denom)
			return input, output, requests, &rate, nil
		}
	}
	return input, output, requests, nil, nil
}

