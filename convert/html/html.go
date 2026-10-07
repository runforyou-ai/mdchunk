// Package html converts HTML to CommonMark with GFM tables.
//
// The encoding comes from a BOM, then Input.Charset, then the first <meta>
// charset declaration before <body>, then UTF-8 or Options.Fallback. Spanned
// table cells are repeated, a table without header cells promotes its first
// row, and relative links resolve against Input.BaseURL.
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
	if declared == "" {
		declared = metaCharset(data)
	}
	text := textdecode.Decode(data, declared, c.opts.Fallback)
	if strings.TrimSpace(text) == "" {
		return convert.Document{}, nil
	}
	options := []converter.ConvertOptionFunc{converter.WithContext(ctx)}
	if in.BaseURL != "" {
		options = append(options, converter.WithDomain(in.BaseURL))
	}
	markdown, err := c.converter.ConvertString(text, options...)
	if err != nil {
		if ctx.Err() != nil {
			return convert.Document{}, ctx.Err()
		}
		return convert.Document{}, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
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
