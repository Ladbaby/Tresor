package store

import (
	"fmt"
	"strings"
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

// ProviderStatsRow is one row of the per-downstream aggregate used by the
// "Top Providers" section of the Dashboard. One row per downstream (id is
// the unique key from the downstreams table), totals summed across every
// model routed through that downstream in the requested range.
//
// Name is the human-friendly display name from the downstreams table (e.g.
// "Anthropic", "OpenAI Responses"). Falls back to DownstreamID when the
// downstream has been deleted from the downstreams table — defensive only;
// under normal operation the FK-on-delete cascade keeps these in sync.
type ProviderStatsRow struct {
	DownstreamID  string `json:"downstream_id"`
	Name          string `json:"name"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheCreation int64  `json:"cache_creation_tokens"`
	CacheRead     int64  `json:"cache_read_tokens"`
	RequestCount  int64  `json:"request_count"`
	CacheHitCount int64  `json:"cache_hit_count"`
}

// IPStatsRow is one row of the per-client-IP aggregate used by the
// "Top IPs by Tokens" section of the Dashboard. One row per client IP,
// totals summed across every downstream/model for that IP in the range.
type IPStatsRow struct {
	ClientIP      string `json:"client_ip"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheCreation int64  `json:"cache_creation_tokens"`
	CacheRead     int64  `json:"cache_read_tokens"`
	RequestCount  int64  `json:"request_count"`
	CacheHitCount int64  `json:"cache_hit_count"`
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

// StatsBatchEntry is one buffered record waiting to be flushed to disk.
// The engine collects these in memory and writes them in bulk via
// BulkRecordUsageStats every flush interval (default 60s).
type StatsBatchEntry struct {
	Bucket        string
	DownstreamID  string
	Model         string
	ClientIP      string // "" means no client IP — skip the ip_usage_stats table
	InputTokens   int64
	OutputTokens  int64
	CacheCreation int64
	CacheRead     int64
}

// BulkRecordUsageStats performs all upserts in a single transaction. This is
// dramatically cheaper than per-request inserts: one fsync instead of N, and
// one round-trip to SQLite instead of N. The engine calls this on a fixed
// interval (default 60s) to amortise the disk I/O across many requests.
//
// Returns the number of rows written. On error, the transaction is rolled
// back and no rows are persisted.
func (s *Store) BulkRecordUsageStats(entries []StatsBatchEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("bulk stats begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// SQLite's max variable count is 32766 (SQLITE_MAX_VARIABLE_NUMBER).
	// Each row uses 9 bind vars, so we cap a single statement at ~3000 rows
	// (9 * 3000 = 27000) to stay safely under the limit.
	const maxRowsPerStmt = 3000

	written := 0
	for offset := 0; offset < len(entries); offset += maxRowsPerStmt {
		end := offset + maxRowsPerStmt
		if end > len(entries) {
			end = len(entries)
		}
		chunk := entries[offset:end]

		// Build "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?), (?, ?, ...), ..."
		var sb strings.Builder
		sb.WriteString(`INSERT INTO usage_stats
			(bucket, downstream_id, model, input_tokens, output_tokens,
			 cache_creation, cache_read, request_count, cache_hit_count)
			VALUES `)
		args := make([]interface{}, 0, len(chunk)*9)
		for i, e := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, ?, 1, ?)")
			bucket := e.Bucket
			if bucket == "" {
				bucket = bucketKey(time.Now())
			}
			cacheHitIncrement := int64(0)
			if e.CacheRead > 0 {
				cacheHitIncrement = 1
			}
			args = append(args,
				bucket, e.DownstreamID, e.Model,
				e.InputTokens, e.OutputTokens,
				e.CacheCreation, e.CacheRead,
				cacheHitIncrement)
		}
		sb.WriteString(`
			ON CONFLICT(bucket, downstream_id, model) DO UPDATE SET
				input_tokens    = input_tokens    + excluded.input_tokens,
				output_tokens   = output_tokens   + excluded.output_tokens,
				cache_creation  = cache_creation  + excluded.cache_creation,
				cache_read      = cache_read      + excluded.cache_read,
				request_count   = request_count   + 1,
				cache_hit_count = cache_hit_count + excluded.cache_hit_count
		`)

		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return written, fmt.Errorf("bulk stats exec (chunk %d-%d): %w", offset, end, err)
		}
		written += len(chunk)
	}

	if err := tx.Commit(); err != nil {
		return written, fmt.Errorf("bulk stats commit: %w", err)
	}
	committed = true
	return written, nil
}

// BulkRecordIPUsageStats performs the per-client-IP upserts in a single
// transaction, mirroring BulkRecordUsageStats. Entries with an empty
// ClientIP are skipped — unattributable traffic is not stored as a
// catch-all "" row. Returns the number of rows written (skipped entries
// are not counted).
func (s *Store) BulkRecordIPUsageStats(entries []StatsBatchEntry) (int, error) {
	filtered := make([]StatsBatchEntry, 0, len(entries))
	for _, e := range entries {
		if e.ClientIP == "" {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) == 0 {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("bulk ip stats begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Each row uses 8 bind vars, so the same 3000-row cap as
	// BulkRecordUsageStats stays safely under SQLite's variable limit.
	const maxRowsPerStmt = 3000

	written := 0
	for offset := 0; offset < len(filtered); offset += maxRowsPerStmt {
		end := offset + maxRowsPerStmt
		if end > len(filtered) {
			end = len(filtered)
		}
		chunk := filtered[offset:end]

		var sb strings.Builder
		sb.WriteString(`INSERT INTO ip_usage_stats
			(bucket, client_ip, input_tokens, output_tokens,
			 cache_creation, cache_read, request_count, cache_hit_count)
			VALUES `)
		args := make([]interface{}, 0, len(chunk)*8)
		for i, e := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, 1, ?)")
			bucket := e.Bucket
			if bucket == "" {
				bucket = bucketKey(time.Now())
			}
			cacheHitIncrement := int64(0)
			if e.CacheRead > 0 {
				cacheHitIncrement = 1
			}
			args = append(args,
				bucket, e.ClientIP,
				e.InputTokens, e.OutputTokens,
				e.CacheCreation, e.CacheRead,
				cacheHitIncrement)
		}
		sb.WriteString(`
			ON CONFLICT(bucket, client_ip) DO UPDATE SET
				input_tokens    = input_tokens    + excluded.input_tokens,
				output_tokens   = output_tokens   + excluded.output_tokens,
				cache_creation  = cache_creation  + excluded.cache_creation,
				cache_read      = cache_read      + excluded.cache_read,
				request_count   = request_count   + 1,
				cache_hit_count = cache_hit_count + excluded.cache_hit_count
		`)

		if _, err := tx.Exec(sb.String(), args...); err != nil {
			return written, fmt.Errorf("bulk ip stats exec (chunk %d-%d): %w", offset, end, err)
		}
		written += len(chunk)
	}

	if err := tx.Commit(); err != nil {
		return written, fmt.Errorf("bulk ip stats commit: %w", err)
	}
	committed = true
	return written, nil
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

// AggregateByProvider returns per-downstream aggregates for the given time
// range, ordered by total tokens (input + output) descending. Unlike
// AggregateStats, this collapses every model in a downstream into a single
// row — it's the rollup used by the Dashboard's "Top Providers by Tokens"
// section.
//
// The query LEFT JOINs against the downstreams table so each row carries
// the human-friendly display name in addition to the unique id. Rows for
// downstreams that no longer exist (the downstreams row was deleted after
// the usage_stats row was written) fall back to DownstreamID for Name.
func (s *Store) AggregateByProvider(q StatsQuery) ([]ProviderStatsRow, error) {
	fromKey := bucketKey(q.From)
	toKey := bucketKey(q.To)
	rows, err := s.db.Query(`
		SELECT u.downstream_id,
		       COALESCE(d.name, u.downstream_id) AS display_name,
		       COALESCE(SUM(u.input_tokens), 0),    COALESCE(SUM(u.output_tokens), 0),
		       COALESCE(SUM(u.cache_creation), 0),
		       COALESCE(SUM(u.cache_read), 0),
		       COALESCE(SUM(u.request_count), 0),
		       COALESCE(SUM(u.cache_hit_count), 0)
		FROM usage_stats u
		LEFT JOIN downstreams d ON d.id = u.downstream_id
		WHERE u.bucket >= ? AND u.bucket <= ?
		GROUP BY u.downstream_id
		ORDER BY (SUM(u.input_tokens) + SUM(u.output_tokens)) DESC
	`, fromKey, toKey)
	if err != nil {
		return nil, fmt.Errorf("aggregate by provider: %w", err)
	}
	defer rows.Close()

	result := []ProviderStatsRow{}
	for rows.Next() {
		var r ProviderStatsRow
		if err := rows.Scan(&r.DownstreamID, &r.Name,
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

// AggregateByIP returns per-client-IP aggregates for the given time range,
// ordered by total tokens (input + output) descending. Unlike AggregateStats,
// this collapses every downstream/model for a client IP into a single row —
// it's the rollup used by the Dashboard's "Top IPs by Tokens" section.
func (s *Store) AggregateByIP(q StatsQuery) ([]IPStatsRow, error) {
	fromKey := bucketKey(q.From)
	toKey := bucketKey(q.To)
	rows, err := s.db.Query(`
		SELECT client_ip,
		       COALESCE(SUM(input_tokens), 0),    COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0),
		       COALESCE(SUM(cache_read), 0),
		       COALESCE(SUM(request_count), 0),
		       COALESCE(SUM(cache_hit_count), 0)
		FROM ip_usage_stats
		WHERE bucket >= ? AND bucket <= ?
		GROUP BY client_ip
		ORDER BY (SUM(input_tokens) + SUM(output_tokens)) DESC
	`, fromKey, toKey)
	if err != nil {
		return nil, fmt.Errorf("aggregate by ip: %w", err)
	}
	defer rows.Close()

	result := []IPStatsRow{}
	for rows.Next() {
		var r IPStatsRow
		if err := rows.Scan(&r.ClientIP, &r.InputTokens, &r.OutputTokens,
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

// PurgeUsageStatsBefore deletes every usage_stats and ip_usage_stats row
// whose bucket key is strictly less than the given cutoff. The bucket key uses the same ISO8601
// hour format as the rest of the package, so callers should pass a value
// from bucketKey(t). Returns the number of rows deleted.
//
// The DELETE uses the idx_usage_bucket index so it does not require a
// full-table scan. Safe to run while the gateway is live — any concurrent
// INSERT ... ON CONFLICT from the flusher for newer buckets is unaffected.
func (s *Store) PurgeUsageStatsBefore(cutoff string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM usage_stats WHERE bucket < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge usage stats before %q: %w", cutoff, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge usage stats rows affected: %w", err)
	}
	// Purge the per-IP rollup under the same retention window so
	// ip_usage_stats can never outlive usage_stats.
	res2, err := s.db.Exec(`DELETE FROM ip_usage_stats WHERE bucket < ?`, cutoff)
	if err != nil {
		return n, fmt.Errorf("purge ip usage stats before %q: %w", cutoff, err)
	}
	n2, err := res2.RowsAffected()
	if err != nil {
		return n, fmt.Errorf("purge ip usage stats rows affected: %w", err)
	}
	return n + n2, nil
}

