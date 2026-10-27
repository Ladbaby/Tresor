package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"tresor/internal/inspect"
	"tresor/internal/store"
)

// boundedResponseCapture retains the original wire payload without allowing
// inspection to grow with the lifetime of a forced stream.
type boundedResponseCapture struct {
	bytes.Buffer
	truncated bool
}

func (b *boundedResponseCapture) Write(p []byte) (int, error) {
	n := len(p)
	room := inspect.MaxBodyBytes - b.Len()
	if n > room {
		b.truncated = true
		p = p[:room]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

type captureReadCloser struct {
	io.Reader
	io.Closer
}

// recordAndCapture is the single funnel for the engine's per-request log
// write. It calls logger.Record (which assigns the entry id) and, when
// the inspector store is attached, snapshots the pre-transformer body
// bytes into the payload store keyed by the same id.
//
// For ordinary forwarding the pair contains pre-transformer client/upstream
// bytes. Integration routes instead pair the original client request with the
// actual client-facing projection; their Flow retains internal upstream bytes.
//
// requestFormat and downstreamFormat are the API formats of the
// incoming request body and the downstream response body, respectively.
// The inspector UI uses them to pick the correct normaliser for each
// side — important when an auto-translator rewrote the body between
// formats and the client's URL path is no longer a reliable hint for
// the response shape.
func (e *Engine) recordAndCapture(entry *RequestLogEntry, reqBody, respBody []byte, reqCT, respCT string, truncResp bool, requestFormat, downstreamFormat string) {
	if entry.deferRecord {
		entry.capture = &pendingCapture{request: append([]byte(nil), reqBody...), response: append([]byte(nil), respBody...), requestType: reqCT, responseType: respCT, truncated: truncResp, requestFormat: requestFormat, downstreamFormat: downstreamFormat, integration: entry.integrationCapture}
		entry.integrationCapture = nil
		return
	}
	c := entry.integrationCapture
	entry.integrationCapture = nil
	e.logger.Record(entry)

	// Persist usage stats — only when payload capture is enabled and usage
	// was parsed from the response. The store is populated incrementally by
	// the request pipeline and streaming handler, so by the time this
	// function is called entry.Usage already holds the merged totals.
	//
	// Instead of writing one SQLite row per request (which produces
	// N fsyncs per N requests, wearing SSDs and adding tail latency),
	// we append to an in-memory buffer that the background flusher
	// drains in a single bulk transaction every statsFlushInterval.
	// Task summaries repeat usage attributed to their underlying generations;
	// expose them in the inspector without counting another model generation.
	if e.payloadStore != nil && entry.Usage != nil && e.store != nil && entry.UsageScope != "task" {
		var input, output, cacheCreation, cacheRead int64
		if entry.Usage.InputTokens != nil {
			input = *entry.Usage.InputTokens
		}
		if entry.Usage.OutputTokens != nil {
			output = *entry.Usage.OutputTokens
		}
		if entry.Usage.CacheCreationTokens != nil {
			cacheCreation = *entry.Usage.CacheCreationTokens
		}
		if entry.Usage.CacheReadTokens != nil {
			cacheRead = *entry.Usage.CacheReadTokens
		}
		// OpenAI's cached_tokens is the equivalent of Anthropic's cache_read.
		if entry.Usage.CachedTokens != nil {
			cacheRead += *entry.Usage.CachedTokens
		}
		var decodeTokens, decodeDuration, decodeCount int64
		if entry.DecodeTokens != nil && entry.DecodeDurationMS != nil {
			decodeTokens = *entry.DecodeTokens
			decodeDuration = entry.decodeDurationNS
			decodeCount = 1
		}
		e.bufferStats(store.StatsBatchEntry{
			DecodeTokens: decodeTokens, DecodeDurationNS: decodeDuration, DecodeRequestCount: decodeCount,
			Bucket:        entry.Timestamp.UTC().Format("2006-01-02T15:00:00Z"),
			DownstreamID:  entry.DownstreamID,
			Model:         entry.ResolvedModel,
			ClientIP:      entry.ClientIP,
			InputTokens:   input,
			OutputTokens:  output,
			CacheCreation: cacheCreation,
			CacheRead:     cacheRead,
		})
	}

	if e.payloadStore == nil {
		return
	}
	captured := inspect.Entry{
		ID:                  entry.ID,
		Timestamp:           entry.Timestamp,
		Path:                entry.Path,
		Method:              entry.Method,
		Model:               entry.Model,
		ResolvedModel:       entry.ResolvedModel,
		DownstreamID:        entry.DownstreamID,
		DownstreamName:      entry.DownstreamName,
		Status:              entry.Status,
		ClientIP:            entry.ClientIP,
		RequestBody:         reqBody,
		ResponseBody:        respBody,
		RequestContentType:  reqCT,
		ResponseContentType: respCT,
		TruncatedResponse:   truncResp,
		RequestFormat:       requestFormat,
		DownstreamFormat:    downstreamFormat,
		DecodeTokens:        entry.DecodeTokens, DecodeDurationMS: entry.DecodeDurationMS,
		DecodeTokensPerSecond: entry.DecodeTokensPerSecond, DecodeUnavailableReason: entry.DecodeUnavailableReason,
	}
	if entry.Usage != nil {
		captured.Usage, _ = json.Marshal(entry.Usage)
	}
	captured.UsageScope = entry.UsageScope
	if c != nil {
		captured.Flow, captured.FlowTruncated = c.events, c.truncated
		captured.TaskID, captured.ExecutionID, captured.InferenceID = c.taskID, c.executionID, c.inferenceID
	}
	e.payloadStore.Add(captured)
	entry.integrationCapture = nil
}
