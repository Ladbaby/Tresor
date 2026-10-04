package engine

import (
	"bytes"
	"io"
	"tresor/internal/inspect"
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
