package engine

// pendingCapture delays only log publication, not client output. Copies bound
// capture buffers because streaming code may release its backing storage.
type pendingCapture struct {
	request, response               []byte
	requestType, responseType       string
	truncated                       bool
	requestFormat, downstreamFormat string
	integration                     *integrationCapture
}

func (e *Engine) publishRequest(entry *RequestLogEntry) {
	entry.deferRecord = false
	if capture := entry.capture; capture != nil {
		entry.capture = nil
		entry.integrationCapture = capture.integration
		e.recordAndCapture(entry, capture.request, capture.response, capture.requestType, capture.responseType, capture.truncated, capture.requestFormat, capture.downstreamFormat)
	} else {
		e.logger.Record(entry)
	}
}
