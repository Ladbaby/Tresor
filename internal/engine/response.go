package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// bufferedResponse retains both representations: upstream bytes drive retry and
// usage detection; transformed bytes are delivered to the client. Capture may
// contain raw SSE instead of the reconstructed JSON when a collector is used.
type bufferedResponse struct {
	body, transformed, captured []byte
	captureType                 string
	truncated                   bool
}

// readBufferedResponse owns upstream reading/closing, not attempt finalization
// or log publication. Those boundaries remain explicit in HandleProxy.
func (e *Engine) readBufferedResponse(resp *http.Response, ctx *PipelineContext, pipeline *Pipeline, cancel context.CancelFunc, clientCtx context.Context, collectStream bool, upstreamContentType string) (*bufferedResponse, *gatewayError) {
	result := &bufferedResponse{}
	var collectedRaw *boundedResponseCapture
	originalRespCT := resp.Header.Get("Content-Type")
	var err error
	if collectStream {
		if e.payloadStore != nil {
			collectedRaw = &boundedResponseCapture{}
			resp.Body = &captureReadCloser{Reader: io.TeeReader(resp.Body, collectedRaw), Closer: resp.Body}
		}
		stop := context.AfterFunc(clientCtx, func() { cancel(); resp.Body.Close() })
		result.body, err = collectExternalPipeline(resp, ctx, pipeline)
		stop()
		if ctx.ExternalCollector == nil || resp.Header.Get("Content-Type") == upstreamContentType {
			resp.Header.Set("Content-Type", "application/json")
		}
		resp.Header.Del("Content-Length")
		resp.Header.Del("Cache-Control")
		resp.Header.Del("X-Accel-Buffering")
	} else {
		result.body, err = io.ReadAll(resp.Body)
	}
	resp.Body.Close()
	cancel()
	if err != nil {
		errMsg := fmt.Sprintf("failed to read upstream response (%d)", resp.StatusCode)
		if len(result.body) > 0 {
			errMsg += ": " + truncateString(string(result.body), 500)
		}
		status := http.StatusBadGateway
		var upstreamErr interface{ UpstreamStatus() int }
		if errors.As(err, &upstreamErr) {
			status = upstreamErr.UpstreamStatus()
			errMsg = err.Error()
		}
		return nil, &gatewayError{status, errMsg, errMsg, "failed to read response", err}
	}

	if e.payloadStore != nil && len(result.body) > 0 {
		capture := &boundedResponseCapture{}
		capture.Write(result.body)
		result.captured, result.truncated = capture.Bytes(), capture.truncated
	}
	if collectedRaw != nil {
		result.captured, result.truncated = collectedRaw.Bytes(), collectedRaw.truncated
	}
	result.captureType = resp.Header.Get("Content-Type")
	if collectStream {
		result.captureType = originalRespCT
	}
	result.transformed, err = ExecuteResponsePipeline(resp, result.body, ctx, pipeline.ResponseSteps)
	if err != nil {
		return nil, &gatewayError{http.StatusBadGateway, "response pipeline error", fmt.Sprintf("response pipeline error: %v", err), "response pipeline error", err}
	}
	return result, nil
}

// prepareBufferedHeaders preserves the existing header-copy boundary before
// recording; writing the actual response remains after attempt cleanup/capture.
func prepareBufferedHeaders(w http.ResponseWriter, resp *http.Response, body []byte) {
	for k, v := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
}

func writeBufferedResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}
