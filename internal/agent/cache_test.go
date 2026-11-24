package agent

import (
	"context"
	"sync/atomic"

	"vedetta/internal/reader"
)

type countingReader struct {
	inner reader.Reader
	calls atomic.Int64
}

func (c *countingReader) Name() string { return c.inner.Name() }
func (c *countingReader) Read(ctx context.Context, u string) (*reader.Document, error) {
	c.calls.Add(1)
	return c.inner.Read(ctx, u)
}
