// Package source reads converter input under the source limit.
package source

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/runforyou-ai/mdchunk/convert"
)

// chunkSize is how much is read between cancellation checks.
const chunkSize = 64 << 10

// Read reads all of in.Reader, checking ctx between reads. maxBytes < 0 means
// unlimited; beyond maxBytes it returns a *convert.LimitError for the source.
func Read(ctx context.Context, in convert.Input, maxBytes int64) ([]byte, error) {
	if in.Reader == nil {
		return nil, errors.New("mdchunk/convert: Input.Reader is nil")
	}
	var data []byte
	buf := make([]byte, chunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := in.Reader.Read(buf)
		data = append(data, buf[:n]...)
		if maxBytes >= 0 && int64(len(data)) > maxBytes {
			return nil, &convert.LimitError{Limit: convert.LimitSource, Max: maxBytes}
		}
		switch {
		case errors.Is(err, io.EOF):
			return data, nil
		case err != nil:
			if in.Name != "" {
				return nil, fmt.Errorf("mdchunk/convert: read %s: %w", in.Name, err)
			}
			return nil, fmt.Errorf("mdchunk/convert: read: %w", err)
		}
	}
}
