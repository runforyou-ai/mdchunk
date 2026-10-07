// Package convert defines how documents become Markdown: the Document and
// Section types, the Converter interface, a Registry that dispatches by
// format, resource Limits and the errors converters report.
//
// Converters live in subpackages so that each pulls in only its own
// dependencies: convert/text, convert/html, convert/csv, convert/docx,
// convert/pptx, convert/xlsx and convert/pdf. Package convert/all registers
// all of them.
//
// Every built-in converter returns valid UTF-8 with LF line endings, no BOM
// and no NUL bytes, and records Sections against the final Markdown, so a
// chunk's byte range from package split can be mapped back to pages, slides
// or sheets with Document.SectionsIn.
package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// Format is a normalised file extension: lower case, without the dot, with
// aliases resolved. Use ParseFormat or FormatOf to obtain one.
type Format string

// Formats of the built-in converters.
const (
	Text     Format = "txt"
	Markdown Format = "md"
	JSON     Format = "json"
	HTML     Format = "html"
	CSV      Format = "csv"
	DOCX     Format = "docx"
	PPTX     Format = "pptx"
	XLSX     Format = "xlsx"
	PDF      Format = "pdf"
)

// formatAliases maps alternative extensions to their canonical format. "yaml"
// has no built-in converter; the alias serves custom ones.
var formatAliases = map[string]Format{
	"markdown": Markdown,
	"htm":      HTML,
	"yml":      "yaml",
	"text":     Text,
}

// ParseFormat normalises a format name or extension such as "PDF", ".pdf" or "markdown".
func ParseFormat(s string) Format {
	s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "."))
	if alias, ok := formatAliases[s]; ok {
		return alias
	}
	return Format(s)
}

// FormatOf returns the format of a file name or path by its extension.
func FormatOf(filename string) (Format, bool) {
	base := filename[strings.LastIndexAny(filename, `/\`)+1:]
	dot := strings.LastIndexByte(base, '.')
	if dot <= 0 || dot == len(base)-1 {
		return "", false
	}
	return ParseFormat(base[dot+1:]), true
}

// Input is one source document.
type Input struct {
	// Reader supplies the document. Converters do not close it.
	Reader io.Reader
	// Name optionally identifies the document in error messages.
	Name string
	// Charset optionally declares the text encoding, for example from an HTTP header.
	Charset string
	// BaseURL optionally resolves relative links in HTML.
	BaseURL string
}

// Document is the Markdown converted from a source document.
type Document struct {
	// Markdown is valid UTF-8 with LF line endings.
	Markdown string
	// Sections locate the source's pages, slides or sheets in Markdown, ordered by Start.
	Sections []Section
}

// SectionKind names the unit of the source a Section stands for.
type SectionKind string

// Section kinds of the built-in converters.
const (
	KindPage  SectionKind = "page"
	KindSlide SectionKind = "slide"
	KindSheet SectionKind = "sheet"
)

// Section is one citable unit of the source and its byte range in Document.Markdown.
type Section struct {
	Kind SectionKind
	// Number is the 1-based position of the unit in the source, counting empty and skipped units.
	Number int
	// Name is the sheet name, slide title or page label when known.
	Name string
	// Start and End delimit the unit's Markdown, including headings generated for it.
	Start, End int
}

// SectionsIn returns a new slice of the sections whose ranges intersect [start, end).
// Empty sections and empty ranges never match.
func (d Document) SectionsIn(start, end int) []Section {
	var sections []Section
	for _, s := range d.Sections {
		if start < end && s.Start < s.End && s.Start < end && start < s.End {
			sections = append(sections, s)
		}
	}
	return sections
}

// Converter converts one source document to Markdown.
type Converter interface {
	Convert(ctx context.Context, in Input) (Document, error)
}

// ConverterFunc adapts a function to the Converter interface.
type ConverterFunc func(ctx context.Context, in Input) (Document, error)

// Convert calls f.
func (f ConverterFunc) Convert(ctx context.Context, in Input) (Document, error) {
	return f(ctx, in)
}

// Registry maps formats to converters. The zero value is ready to use and safe
// for concurrent use. Formats are normalised with ParseFormat; a later
// registration replaces an earlier one. A Registry never closes its converters.
type Registry struct {
	mu         sync.RWMutex
	converters map[Format]Converter
}

// Register makes c the converter for formats. It panics if c is nil.
func (r *Registry) Register(c Converter, formats ...Format) {
	if c == nil {
		panic("mdchunk/convert: Register with a nil Converter")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.converters == nil {
		r.converters = make(map[Format]Converter)
	}
	for _, f := range formats {
		r.converters[ParseFormat(string(f))] = c
	}
}

// Lookup returns the converter registered for f.
func (r *Registry) Lookup(f Format) (Converter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.converters[ParseFormat(string(f))]
	return c, ok
}

// Formats returns the registered formats in sorted order.
func (r *Registry) Formats() []Format {
	r.mu.RLock()
	defer r.mu.RUnlock()
	formats := make([]Format, 0, len(r.converters))
	for f := range r.converters {
		formats = append(formats, f)
	}
	slices.Sort(formats)
	return formats
}

// Convert converts in with the converter registered for f. It returns an error
// wrapping ErrUnsupported when no converter is registered.
func (r *Registry) Convert(ctx context.Context, f Format, in Input) (Document, error) {
	c, ok := r.Lookup(f)
	if !ok {
		return Document{}, fmt.Errorf("%w: %q", ErrUnsupported, ParseFormat(string(f)))
	}
	return c.Convert(ctx, in)
}

// Default limits.
const (
	DefaultMaxBytes         = 32 << 20
	DefaultMaxExpandedBytes = 128 << 20
	DefaultMaxOutputBytes   = 64 << 20
)

// Limits bounds the resources one conversion may use. Every built-in
// converter's Options embeds Limits; Registry.Convert applies none of its own.
// Zero fields take the defaults; negative fields disable the limit.
type Limits struct {
	// MaxBytes bounds the source bytes read.
	MaxBytes int64
	// MaxExpandedBytes bounds the decompressed bytes read from archive formats.
	MaxExpandedBytes int64
	// MaxOutputBytes bounds the Markdown produced.
	MaxOutputBytes int64
}

// Effective returns the limits with defaults applied; -1 means unlimited.
func (l Limits) Effective() Limits {
	pick := func(value, fallback int64) int64 {
		switch {
		case value == 0:
			return fallback
		case value < 0:
			return -1
		}
		return value
	}
	return Limits{
		MaxBytes:         pick(l.MaxBytes, DefaultMaxBytes),
		MaxExpandedBytes: pick(l.MaxExpandedBytes, DefaultMaxExpandedBytes),
		MaxOutputBytes:   pick(l.MaxOutputBytes, DefaultMaxOutputBytes),
	}
}

// Errors reported by converters. Converters wrap them with %w and keep the
// underlying cause in the chain.
var (
	ErrUnsupported = errors.New("mdchunk/convert: unsupported format")
	ErrEncrypted   = errors.New("mdchunk/convert: encrypted document")
	ErrCorrupt     = errors.New("mdchunk/convert: malformed document")
	ErrTooLarge    = errors.New("mdchunk/convert: limit exceeded")
	ErrClosed      = errors.New("mdchunk/convert: converter closed")
)

// Limit names used by LimitError.
const (
	LimitSource   = "source"
	LimitExpanded = "expanded"
	LimitOutput   = "output"
)

// LimitError reports that a conversion exceeded one of its Limits.
// errors.Is(err, ErrTooLarge) reports true for it.
type LimitError struct {
	// Limit is LimitSource, LimitExpanded or LimitOutput.
	Limit string
	// Max is the limit in bytes.
	Max int64
}

// Error describes the exceeded limit.
func (e *LimitError) Error() string {
	return fmt.Sprintf("mdchunk/convert: %s exceeds %d bytes", e.Limit, e.Max)
}

// Is reports whether target is ErrTooLarge.
func (e *LimitError) Is(target error) bool {
	return target == ErrTooLarge
}
