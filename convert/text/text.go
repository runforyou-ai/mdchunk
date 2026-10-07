// Package text converts plain text, Markdown and JSON.
//
// Plain text and Markdown are decoded and normalised but otherwise emitted
// unchanged, so the splitter reads plain text as Markdown, as most plain text
// is. JSON is wrapped in a code fence so it is only split between lines.
package text

import (
	"context"

	"golang.org/x/text/encoding"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/source"
	"github.com/runforyou-ai/mdchunk/internal/textdecode"
)

// Options configures the text converters. The zero value is the default.
type Options struct {
	convert.Limits
	// Fallback decodes input that is neither declared nor valid UTF-8, for
	// example simplifiedchinese.GB18030. Nil replaces invalid bytes with U+FFFD.
	Fallback encoding.Encoding
}

// New returns a converter for plain text and Markdown.
func New(opts Options) convert.Converter {
	return converter{opts: opts}
}

// JSON returns a converter that wraps JSON in a json code fence.
func JSON(opts Options) convert.Converter {
	return converter{opts: opts, fence: "json"}
}

// converter decodes text and optionally fences it.
type converter struct {
	opts  Options
	fence string
}

// Convert decodes in and emits it as Markdown.
func (c converter) Convert(ctx context.Context, in convert.Input) (convert.Document, error) {
	limits := c.opts.Effective()
	data, err := source.Read(ctx, in, limits.MaxBytes)
	if err != nil {
		return convert.Document{}, err
	}
	text := textdecode.Normalize(textdecode.Decode(data, in.Charset, c.opts.Fallback))
	if text == "" {
		return convert.Document{}, nil
	}
	if c.fence != "" {
		text = mdwrite.Fence(c.fence, text)
	}
	w := mdwrite.New(limits.MaxOutputBytes)
	w.WriteString(text)
	if err := w.Err(); err != nil {
		return convert.Document{}, err
	}
	return convert.Document{Markdown: w.String()}, nil
}
