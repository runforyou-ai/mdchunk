// Package csv converts comma-separated values to a GFM table.
//
// The first record is the header unless Options.NoHeader is set, in which
// case every record is a body row under an empty header row of the same
// width. Rows are padded to the widest record; empty records are skipped.
package csv

import (
	"context"
	stdcsv "encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/text/encoding"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/source"
	"github.com/runforyou-ai/mdchunk/internal/textdecode"
)

// Options configures the CSV converter. The zero value is the default.
type Options struct {
	convert.Limits
	// Fallback decodes input that is neither declared nor valid UTF-8, for
	// example simplifiedchinese.GB18030. Nil replaces invalid bytes with U+FFFD.
	Fallback encoding.Encoding
	// Comma is the field delimiter; zero means ','.
	Comma rune
	// NoHeader treats the first record as data.
	NoHeader bool
	// LazyQuotes accepts quotes in unquoted fields and unescaped quotes in quoted fields.
	LazyQuotes bool
}

// Converter converts CSV. It is safe for concurrent use.
type Converter struct {
	opts Options
}

// New returns a CSV converter.
func New(opts Options) *Converter {
	return &Converter{opts: opts}
}

// Convert converts in to a Markdown table.
func (c *Converter) Convert(ctx context.Context, in convert.Input) (convert.Document, error) {
	limits := c.opts.Effective()
	data, err := source.Read(ctx, in, limits.MaxBytes)
	if err != nil {
		return convert.Document{}, err
	}
	reader := stdcsv.NewReader(strings.NewReader(textdecode.Normalize(textdecode.Decode(data, in.Charset, c.opts.Fallback))))
	reader.FieldsPerRecord, reader.LazyQuotes, reader.ReuseRecord = -1, c.opts.LazyQuotes, true
	if c.opts.Comma != 0 {
		reader.Comma = c.opts.Comma
	}
	table := mdwrite.NewTable(limits.MaxOutputBytes)
	for count := 0; ; count++ {
		if count%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return convert.Document{}, err
			}
		}
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return convert.Document{}, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
		}
		if err := table.Add(record); err != nil {
			return convert.Document{}, err
		}
	}
	w := mdwrite.New(limits.MaxOutputBytes)
	w.WriteString(table.Markdown(!c.opts.NoHeader))
	if err := w.Err(); err != nil {
		return convert.Document{}, err
	}
	return convert.Document{Markdown: w.String()}, nil
}
