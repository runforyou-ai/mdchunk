// Package pdf converts PDF documents to Markdown with PDFium compiled to
// WebAssembly, so it needs no cgo or system libraries.
//
// Each page becomes a convert.KindPage section. Text lines are grouped by
// font size: the size with the most text is body text, and larger short
// lines become headings unless Options.NoHeadings is set. Paragraphs break
// where line spacing grows or the font size changes. Reading order follows
// PDFium's text order; multi-column layout and table recovery are not
// attempted. A PDF without a text layer yields empty Markdown and one empty
// section per page. Documents that need a password to open return
// convert.ErrEncrypted; documents restricted only by an owner password convert
// normally, and permission flags are not checked.
//
// Options.MaxPages and Options.MaxPageChars bound the pages of a document and
// the characters of one page, and line text counts against
// Limits.MaxOutputBytes as it is read. PDFium parses each page in one call
// that cannot be interrupted, before its characters can be counted.
package pdf

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/klippa-app/go-pdfium"
	pdfiumerrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/source"
)

// Heading detection: a heading line's font is at least headingScale times the
// body size and it has at most headingMaxRunes runes; sizes beyond
// headingLevels map to the lowest level.
const (
	headingScale    = 1.15
	headingMaxRunes = 60
	headingLevels   = 3
)

// Default page limits.
const (
	DefaultMaxPages     = 10000
	DefaultMaxPageChars = 1 << 20
)

// Options configures the PDF converter. The zero value is the default.
type Options struct {
	convert.Limits
	// Workers bounds concurrent conversions and PDFium instances; zero means 2.
	Workers int
	// NoHeadings renders every line as body text.
	NoHeadings bool
	// MaxPages bounds the pages of a document. Zero means DefaultMaxPages;
	// negative means unlimited.
	MaxPages int
	// MaxPageChars bounds the characters PDFium reports for one page,
	// including the spaces and line breaks it generates. Zero means
	// DefaultMaxPageChars; negative means unlimited.
	MaxPageChars int
}

// Converter converts PDF documents. It is safe for concurrent use and must be
// closed to release its PDFium instances.
type Converter struct {
	opts  Options
	slots chan struct{}
	done  chan struct{}

	mu       sync.Mutex
	closed   bool
	released chan struct{} // closed once Close has released the instances
	closeErr error
	pool     pdfium.Pool
	inFlight sync.WaitGroup
}

// New returns a PDF converter. PDFium starts on first use.
func New(opts Options) *Converter {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	return &Converter{opts: opts, slots: make(chan struct{}, opts.Workers), done: make(chan struct{}), released: make(chan struct{})}
}

// Close releases the PDFium instances after conversions in flight finish.
// Calls waiting for a worker, and later calls, return convert.ErrClosed.
// Every call to Close returns once the instances are released.
func (c *Converter) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.released
		return c.closeErr
	}
	c.closed = true
	close(c.done)
	c.mu.Unlock()
	c.inFlight.Wait()
	c.mu.Lock()
	if c.pool != nil {
		c.closeErr = c.pool.Close()
		c.pool = nil
	}
	c.mu.Unlock()
	close(c.released)
	return c.closeErr
}

// acquire takes a worker slot and returns the PDFium pool, starting it if needed.
func (c *Converter) acquire(ctx context.Context) (pdfium.Pool, error) {
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, convert.ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		<-c.slots
		return nil, convert.ErrClosed
	}
	if c.pool == nil {
		pool, err := webassembly.Init(webassembly.Config{MaxTotal: c.opts.Workers})
		if err != nil {
			<-c.slots
			return nil, fmt.Errorf("mdchunk/convert/pdf: start PDFium: %w", err)
		}
		c.pool = pool
	}
	c.inFlight.Add(1)
	return c.pool, nil
}

// release returns a worker slot.
func (c *Converter) release() {
	c.inFlight.Done()
	<-c.slots
}

// Convert converts in to Markdown with one section per page.
func (c *Converter) Convert(ctx context.Context, in convert.Input) (convert.Document, error) {
	pool, err := c.acquire(ctx)
	if err != nil {
		return convert.Document{}, err
	}
	defer c.release()
	limits := c.opts.Effective()
	data, err := source.Read(ctx, in, limits.MaxBytes)
	if err != nil {
		return convert.Document{}, err
	}
	if len(data) == 0 {
		return convert.Document{}, fmt.Errorf("%w: empty file", convert.ErrCorrupt)
	}
	maxPages, maxPageChars := c.opts.MaxPages, c.opts.MaxPageChars
	if maxPages == 0 {
		maxPages = DefaultMaxPages
	}
	if maxPageChars == 0 {
		maxPageChars = DefaultMaxPageChars
	}
	lines, pages, err := extract(ctx, pool, data, extractLimits{
		pages: maxPages, pageChars: maxPageChars, output: limits.MaxOutputBytes,
	})
	if err != nil {
		return convert.Document{}, err
	}
	doc, err := render(lines, pages, !c.opts.NoHeadings, limits.MaxOutputBytes)
	if err != nil {
		return convert.Document{}, err
	}
	if err := ctx.Err(); err != nil {
		return convert.Document{}, err
	}
	return doc, nil
}

// line is a line of text on a page in PDF points, y axis pointing up.
type line struct {
	page        int
	text        string
	size        float64
	top, bottom float64
}

// extractLimits bound extraction; negative fields are unlimited.
type extractLimits struct {
	pages, pageChars int
	// output bounds the bytes of line text, which the Markdown always exceeds.
	output int64
}

// extract reads the text lines of every page and the page count.
func extract(ctx context.Context, pool pdfium.Pool, data []byte, limits extractLimits) ([]line, int, error) {
	instance, err := pool.GetInstanceWithContext(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = instance.Close() }()
	document, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	switch {
	case errors.Is(err, pdfiumerrors.ErrPassword):
		return nil, 0, fmt.Errorf("%w: %w", convert.ErrEncrypted, err)
	case err != nil:
		return nil, 0, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	defer func() {
		_, _ = instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: document.Document})
	}()
	count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: document.Document})
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	if limits.pages >= 0 && count.PageCount > limits.pages {
		return nil, 0, &convert.LimitError{Limit: convert.LimitPages, Max: int64(limits.pages)}
	}
	b := &lineBuilder{output: limits.output, max: limits.output}
	for index := range count.PageCount {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if err := readPage(ctx, instance, document.Document, index, limits.pageChars, b); err != nil {
			return nil, 0, err
		}
	}
	return b.lines, count.PageCount, nil
}

// readPage adds the lines of the page at index to b, following PDFium's text
// order: CRLF pairs in the character stream end lines, the spaces it
// generates or reads separate words, and other control characters count as
// spaces. Each character is read once. Cancellation is checked between
// characters in batches.
func readPage(ctx context.Context, instance pdfium.Pdfium, document references.FPDF_DOCUMENT, index, maxChars int, b *lineBuilder) error {
	corrupt := func(err error) error {
		return fmt.Errorf("%w: page %d: %w", convert.ErrCorrupt, index+1, err)
	}
	text, err := instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{
		Page: requests.Page{ByIndex: &requests.PageByIndex{Document: document, Index: index}},
	})
	if err != nil {
		return corrupt(err)
	}
	defer func() { _, _ = instance.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: text.TextPage}) }()
	count, err := instance.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: text.TextPage})
	if err != nil {
		return corrupt(err)
	}
	if maxChars >= 0 && count.Count > maxChars {
		return &convert.LimitError{Limit: convert.LimitPageChars, Max: int64(maxChars)}
	}
	b.page, b.open, b.space = index, false, false
	carriageReturn := false
	for i := range count.Count {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		char, err := instance.FPDFText_GetUnicode(&requests.FPDFText_GetUnicode{TextPage: text.TextPage, Index: i})
		if err != nil {
			return corrupt(err)
		}
		r, s := utf8.RuneError, ""
		if char.Unicode != 0 {
			s = string(rune(char.Unicode))
			r, _ = utf8.DecodeRuneInString(s)
		}
		// A CR is a line end when an LF follows it, and a control character otherwise.
		if carriageReturn {
			carriageReturn = false
			if r == '\n' {
				b.finish()
				continue
			}
			b.space = b.space || b.open
		}
		if s == "\r" {
			carriageReturn = true
			continue
		}
		if s == "" || unicode.IsSpace(r) || unicode.IsControl(r) {
			b.space = b.space || b.open
			continue
		}
		size, err := instance.FPDFText_GetFontSize(&requests.FPDFText_GetFontSize{TextPage: text.TextPage, Index: i})
		if err != nil {
			return corrupt(err)
		}
		rendered := size.FontSize
		if matrix, err := instance.FPDFText_GetMatrix(&requests.FPDFText_GetMatrix{TextPage: text.TextPage, Index: i}); err == nil {
			rendered *= math.Sqrt(float64(matrix.Matrix.C)*float64(matrix.Matrix.C) + float64(matrix.Matrix.D)*float64(matrix.Matrix.D))
		}
		box, err := instance.FPDFText_GetCharBox(&requests.FPDFText_GetCharBox{TextPage: text.TextPage, Index: i})
		if err != nil {
			return corrupt(err)
		}
		if err := b.add(s, rendered, box.Top, box.Bottom); err != nil {
			return err
		}
	}
	b.finish()
	return nil
}

// lineBuilder groups characters into lines. A line's font size is the size
// of most of its characters and its box encloses theirs.
type lineBuilder struct {
	lines       []line
	page        int
	text        []byte
	weights     map[float64]int
	open, space bool
	// output is what is left of the bytes line text may take, negative when
	// unlimited; max is the limit errors report.
	output, max int64
}

// add appends a character of text, font size and vertical extent to the open
// line, opening one if needed.
func (b *lineBuilder) add(text string, size, top, bottom float64) error {
	if !b.open {
		b.lines = append(b.lines, line{page: b.page, top: top, bottom: bottom})
		b.text, b.open, b.space = b.text[:0], true, false
		if b.weights == nil {
			b.weights = map[float64]int{}
		}
	}
	grow := int64(len(text))
	if b.space {
		grow++
	}
	if b.output >= 0 {
		if grow > b.output {
			return &convert.LimitError{Limit: convert.LimitOutput, Max: b.max}
		}
		b.output -= grow
	}
	l := &b.lines[len(b.lines)-1]
	if b.space {
		b.text = append(b.text, ' ')
		b.space = false
	}
	b.text = append(b.text, text...)
	b.weights[size]++
	l.top, l.bottom = max(l.top, top), min(l.bottom, bottom)
	return nil
}

// finish closes the open line, if any.
func (b *lineBuilder) finish() {
	if !b.open {
		return
	}
	l := &b.lines[len(b.lines)-1]
	l.text = string(b.text)
	best := -1
	for size, count := range b.weights {
		if count > best || count == best && size < l.size {
			l.size, best = size, count
		}
	}
	clear(b.weights)
	b.open = false
}

// render writes lines as Markdown with one section per page.
func render(lines []line, pages int, headings bool, maxOutput int64) (convert.Document, error) {
	weights := map[float64]int{}
	for i := range lines {
		lines[i].text = strings.TrimSpace(lines[i].text)
		lines[i].size = math.Round(lines[i].size*2) / 2
		weights[lines[i].size] += utf8.RuneCountInString(lines[i].text)
	}
	body, weight := 0.0, -1
	for size, count := range weights {
		if count > weight || count == weight && size < body {
			body, weight = size, count
		}
	}
	// Larger sizes of short lines map to heading levels from largest to smallest.
	var sizes []float64
	heading := make([]bool, len(lines))
	for i, l := range lines {
		heading[i] = headings && body > 0 && l.size >= body*headingScale &&
			utf8.RuneCountInString(l.text) <= headingMaxRunes && strings.IndexFunc(l.text, unicode.IsLetter) >= 0
		if heading[i] && !slices.Contains(sizes, l.size) {
			sizes = append(sizes, l.size)
		}
	}
	slices.SortFunc(sizes, func(a, b float64) int { return cmp.Compare(b, a) })

	w := mdwrite.New(maxOutput)
	sections := make([]convert.Section, pages)
	for page := range pages {
		sections[page] = convert.Section{Kind: convert.KindPage, Number: page + 1, Start: -1}
	}
	for i, l := range lines {
		continued := false
		if i > 0 {
			previous := lines[i-1]
			separator := "\n\n"
			// Adjacent lines on a page with the same size and role and normal spacing form one paragraph;
			// consecutive heading lines of one level form one heading.
			if previous.page == l.page && previous.size == l.size && heading[i-1] == heading[i] &&
				previous.bottom-l.top <= (previous.top-previous.bottom)*0.8 {
				separator = "\n"
				if heading[i] {
					separator, continued = " ", true
				}
			}
			w.WriteString(separator)
		}
		if sections[l.page].Start < 0 {
			sections[l.page].Start = w.Len()
		}
		switch {
		case heading[i] && !continued:
			w.WriteString(strings.Repeat("#", min(slices.Index(sizes, l.size)+1, headingLevels)) + " " + l.text)
		case heading[i]:
			w.WriteString(l.text)
		default:
			w.WriteString(mdwrite.Paragraph(l.text))
		}
		sections[l.page].End = w.Len()
	}
	if err := w.Err(); err != nil {
		return convert.Document{}, err
	}
	// Pages without text get an empty section where the next page's text starts.
	position := w.Len()
	for page := pages - 1; page >= 0; page-- {
		if sections[page].Start < 0 {
			sections[page].Start, sections[page].End = position, position
		}
		position = sections[page].Start
	}
	return convert.Document{Markdown: w.String(), Sections: sections}, nil
}
