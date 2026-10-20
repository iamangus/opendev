// Package limits bounds subprocess output without blocking the child on a full buffer.
package limits

import "bytes"

type Buffer struct {
	bytes.Buffer
	Limit     int
	Truncated bool
}

func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.Limit - b.Len()
	if remaining < 0 {
		remaining = 0
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.Truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (b *Buffer) String() string {
	if b.Truncated {
		return b.Buffer.String() + "\n[output truncated; narrow the requested scope]\n"
	}
	return b.Buffer.String()
}
