// Package all registers every built-in converter in one Registry.
//
// Importing it compiles every converter's dependencies, including PDFium
// WebAssembly and excelize. To keep a binary small, build a convert.Registry
// with only the converters you need.
package all

import (
	"cmp"

	"golang.org/x/text/encoding"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/convert/csv"
	"github.com/runforyou-ai/mdchunk/convert/docx"
	"github.com/runforyou-ai/mdchunk/convert/html"
	"github.com/runforyou-ai/mdchunk/convert/pdf"
	"github.com/runforyou-ai/mdchunk/convert/pptx"
	"github.com/runforyou-ai/mdchunk/convert/text"
	"github.com/runforyou-ai/mdchunk/convert/xlsx"
)

// Options configures every built-in converter. Each field of Limits applies to
// a converter whose own Limits leave that field zero; Fallback applies to a
// converter whose own Fallback is nil.
type Options struct {
	Limits   convert.Limits
	Fallback encoding.Encoding

	Text text.Options
	HTML html.Options
	CSV  csv.Options
	DOCX docx.Options
	PPTX pptx.Options
	XLSX xlsx.Options
	PDF  pdf.Options
}

// Registry is a convert.Registry holding every built-in converter. Close
// releases the converters New created; converters registered later belong to
// their caller.
type Registry struct {
	*convert.Registry
	pdf *pdf.Converter
}

// New returns a Registry with txt, md, json, html, csv, docx, pptx, xlsx and pdf registered.
func New(opts Options) *Registry {
	limits := func(own convert.Limits) convert.Limits {
		return convert.Limits{
			MaxBytes:         cmp.Or(own.MaxBytes, opts.Limits.MaxBytes),
			MaxExpandedBytes: cmp.Or(own.MaxExpandedBytes, opts.Limits.MaxExpandedBytes),
			MaxOutputBytes:   cmp.Or(own.MaxOutputBytes, opts.Limits.MaxOutputBytes),
		}
	}
	fallback := func(own encoding.Encoding) encoding.Encoding {
		if own == nil {
			return opts.Fallback
		}
		return own
	}
	opts.Text.Limits, opts.Text.Fallback = limits(opts.Text.Limits), fallback(opts.Text.Fallback)
	opts.HTML.Limits, opts.HTML.Fallback = limits(opts.HTML.Limits), fallback(opts.HTML.Fallback)
	opts.CSV.Limits, opts.CSV.Fallback = limits(opts.CSV.Limits), fallback(opts.CSV.Fallback)
	opts.DOCX.Limits = limits(opts.DOCX.Limits)
	opts.PPTX.Limits = limits(opts.PPTX.Limits)
	opts.XLSX.Limits = limits(opts.XLSX.Limits)
	opts.PDF.Limits = limits(opts.PDF.Limits)

	r := &Registry{Registry: &convert.Registry{}, pdf: pdf.New(opts.PDF)}
	r.Register(text.New(opts.Text), convert.Text, convert.Markdown)
	r.Register(text.JSON(opts.Text), convert.JSON)
	r.Register(html.New(opts.HTML), convert.HTML)
	r.Register(csv.New(opts.CSV), convert.CSV)
	r.Register(docx.New(opts.DOCX), convert.DOCX)
	r.Register(pptx.New(opts.PPTX), convert.PPTX)
	r.Register(xlsx.New(opts.XLSX), convert.XLSX)
	r.Register(r.pdf, convert.PDF)
	return r
}

// Close releases the PDF converter's PDFium instances. It is idempotent.
func (r *Registry) Close() error {
	return r.pdf.Close()
}
