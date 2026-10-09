// Package html converts HTML to CommonMark with GFM tables.
//
// The encoding comes from a BOM, then Input.Charset when it names a known
// encoding, then the first <meta> charset declaration before <body> (UTF-16
// read as UTF-8, as browsers do), then UTF-8 or Options.Fallback. Tables,
// innermost first, are expanded to rectangular grids before conversion: spans
// (clamped to the HTML standard's limits and kept within their row group)
// repeat their cell, short rows are padded, and a heuristic score of the
// copies must fit Limits.MaxExpandedBytes; a table containing another table
// is left to the converter, which renders it as text around the inner table.
// Block quotes and lists nested deeper than 8 levels render at the eighth. A
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
	"golang.org/x/net/html/atom"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"

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
		// A <meta> that could be read as ASCII cannot be UTF-16; as in the
		// HTML standard's prescan, such a declaration means UTF-8.
		if e := textdecode.Lookup(declared); e != nil {
			if name, _ := htmlindex.Name(e); name == "utf-16le" || name == "utf-16be" {
				declared = "utf-8"
			}
		}
	}
	text := textdecode.Decode(data, declared, c.opts.Fallback)
	if strings.TrimSpace(text) == "" {
		return convert.Document{}, nil
	}
	doc, err := xhtml.Parse(strings.NewReader(text))
	if err != nil {
		return convert.Document{}, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	limitNesting(doc)
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

// maxNesting is the deepest nesting of block quotes and lists that is kept.
// Each level prefixes or indents every line inside it, and the converter
// rewrites the inner Markdown once per level.
const maxNesting = 8

// nesting are the elements whose nesting maxNesting bounds.
var nesting = map[atom.Atom]bool{atom.Blockquote: true, atom.Ul: true, atom.Ol: true, atom.Menu: true}

// limitNesting turns block quotes and lists nested deeper than maxNesting,
// and the items of such lists, into div elements, so their content renders
// at the deepest kept level.
func limitNesting(doc *xhtml.Node) {
	type frame struct {
		node  *xhtml.Node
		depth int
	}
	stack := []frame{{doc, 0}}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for child := current.node.LastChild; child != nil; child = child.PrevSibling {
			depth := current.depth
			if child.Type == xhtml.ElementNode && nesting[child.DataAtom] {
				depth++
				if depth > maxNesting {
					flattened := child.DataAtom != atom.Blockquote
					child.DataAtom, child.Data = atom.Div, "div"
					for item := child.FirstChild; item != nil && flattened; item = item.NextSibling {
						if item.Type == xhtml.ElementNode && item.DataAtom == atom.Li {
							item.DataAtom, item.Data = atom.Div, "div"
						}
					}
				}
			}
			stack = append(stack, frame{child, depth})
		}
	}
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
