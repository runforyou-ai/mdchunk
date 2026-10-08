// Package html converts HTML to CommonMark with GFM tables.
//
// The encoding comes from a BOM, then Input.Charset when it names a known
// encoding, then the first <meta> charset declaration before <body>, then
// UTF-8 or Options.Fallback. Tables, innermost first, are expanded to
// rectangular grids before conversion: spans (clamped to the HTML standard's
// limits and kept within their row group) repeat their cell, short rows are
// padded, and a heuristic score of the copies must fit
// Limits.MaxExpandedBytes; a table containing another table is left to the
// converter, which renders it as text around the inner table. A
// table without header cells promotes its first row, and relative links
// resolve against Input.BaseURL.
package html

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	xhtml "golang.org/x/net/html"
	"golang.org/x/text/encoding"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/source"
	"github.com/runforyou-ai/mdchunk/internal/textdecode"
)

// Options configures the HTML converter. The zero value is the default.
type Options struct {
	convert.Limits
	// Fallback decodes input that is neither declared nor valid UTF-8, for
	// example simplifiedchinese.GB18030. Nil replaces invalid bytes with U+FFFD.
	Fallback encoding.Encoding
}

// Converter converts HTML. It is safe for concurrent use.
type Converter struct {
	opts      Options
	converter *converter.Converter
}

// New returns an HTML converter.
func New(opts Options) *Converter {
	return &Converter{opts: opts, converter: converter.NewConverter(converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(
			table.WithSpanCellBehavior(table.SpanBehaviorMirror),
			table.WithNewlineBehavior(table.NewlineBehaviorPreserve),
			table.WithCellPaddingBehavior(table.CellPaddingBehaviorMinimal),
			table.WithHeaderPromotion(true),
		),
	))}
}

// Convert converts in to Markdown.
func (c *Converter) Convert(ctx context.Context, in convert.Input) (convert.Document, error) {
	limits := c.opts.Effective()
	data, err := source.Read(ctx, in, limits.MaxBytes)
	if err != nil {
		return convert.Document{}, err
	}
	declared := in.Charset
	if textdecode.Lookup(declared) == nil {
		declared = metaCharset(data)
	}
	text := textdecode.Decode(data, declared, c.opts.Fallback)
	if strings.TrimSpace(text) == "" {
		return convert.Document{}, nil
	}
	doc, err := xhtml.Parse(strings.NewReader(text))
	if err != nil {
		return convert.Document{}, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	if err := prepareTables(doc, limits.MaxExpandedBytes, in.BaseURL); err != nil {
		return convert.Document{}, err
	}
	options := []converter.ConvertOptionFunc{converter.WithContext(ctx)}
	if in.BaseURL != "" {
		options = append(options, converter.WithDomain(in.BaseURL))
	}
	output, err := c.converter.ConvertNode(doc, options...)
	// The library does not observe ctx, so cancellation is checked once it returns.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return convert.Document{}, ctxErr
	}
	if err != nil {
		return convert.Document{}, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	markdown := string(output)
	w := mdwrite.New(limits.MaxOutputBytes)
	w.WriteString(textdecode.Normalize(markdown))
	if err := w.Err(); err != nil {
		return convert.Document{}, err
	}
	return convert.Document{Markdown: w.String()}, nil
}

// metaCharset returns the charset declared by the first <meta> before <body>, or "".
func metaCharset(data []byte) string {
	tokenizer := xhtml.NewTokenizer(bytes.NewReader(data))
	for {
		switch tokenizer.Next() {
		case xhtml.ErrorToken:
			return ""
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			token := tokenizer.Token()
			if token.Data == "body" {
				return ""
			}
			if token.Data != "meta" {
				continue
			}
			name, content, contentType := "", "", false
			for _, attr := range token.Attr {
				switch attr.Key {
				case "charset":
					name = attr.Val
				case "content":
					content = attr.Val
				case "http-equiv":
					contentType = strings.EqualFold(attr.Val, "content-type")
				}
			}
			// The http-equiv form declares the charset as a media type parameter.
			if _, params, err := mime.ParseMediaType(content); name == "" && contentType && err == nil {
				name = params["charset"]
			}
			if name != "" {
				return name
			}
		}
	}
}
