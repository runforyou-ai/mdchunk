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
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/responses"
	"github.com/klippa-app/go-pdfium/webassembly"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/source"
)

// Heading detection: a heading line's font is at least headingScale times the
// body size and it has at most headingMaxRunes runes; sizes beyond
// headingLevels map to the lowest level. wideRune starts the CJK ranges, whose
// text blocks join without a space.
const (
	headingScale    = 1.15
	headingMaxRunes = 60
	headingLevels   = 3
	wideRune        = '\u2e80'
)

// Options configures the PDF converter. The zero value is the default.
type Options struct {
	convert.Limits
	// Workers bounds concurrent conversions and PDFium instances; zero means 2.
	Workers int
	// NoHeadings renders every line as body text.
	NoHeadings bool
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
	lines, pages, err := extract(ctx, pool, data)
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
	page                     int
	text                     string
	size                     float64
	left, right, top, bottom float64
}

// height returns the line's height in points.
func (l line) height() float64 {
	return l.top - l.bottom
}

// extract reads the text lines of every page and the page count.
func extract(ctx context.Context, pool pdfium.Pool, data []byte) ([]line, int, error) {
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
	var lines []line
	for index := range count.PageCount {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		page, err := instance.GetPageTextStructured(&requests.GetPageTextStructured{
			Page:                   requests.Page{ByIndex: &requests.PageByIndex{Document: document.Document, Index: index}},
			Mode:                   requests.GetPageTextStructuredModeChars,
			CollectFontInformation: true,
		})
		if err != nil {
			return nil, 0, fmt.Errorf("%w: page %d: %w", convert.ErrCorrupt, index+1, err)
		}
		lines = appendChars(lines, index, page.Chars)
	}
	// Extraction does not observe ctx, so cancellation is checked once it is done.
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return lines, count.PageCount, nil
}

// appendChars groups a page's characters, in PDFium's text order, into lines.
// Each character is read once; control characters count as spaces. A
// character joins the current line when its box overlaps the line's
// vertically and it does not move back to the left.
// A line's font size is the size of most of its characters.
func appendChars(lines []line, page int, chars []*responses.GetPageTextStructuredChar) []line {
	first := len(lines)
	weights := map[float64]int{}
	var text []byte
	var last rune
	finish := func() {
		if len(lines) == first {
			return
		}
		l := &lines[len(lines)-1]
		l.text = string(text)
		best := -1
		for size, count := range weights {
			if count > best || count == best && size < l.size {
				l.size, best = size, count
			}
		}
		clear(weights)
	}
	space := false
	for _, char := range chars {
		r, _ := utf8.DecodeRuneInString(char.Text)
		if char.Text == "" || unicode.IsSpace(r) || unicode.IsControl(r) {
			space = space || len(lines) > first
			continue
		}
		size := 0.0
		if char.FontInformation != nil {
			size = char.FontInformation.RenderedSize
		}
		position := char.PointPosition
		current := len(lines) - 1
		if current >= first {
			l := &lines[current]
			overlap := min(position.Top, l.top) - max(position.Bottom, l.bottom)
			sameLine := overlap > 0 && position.Left >= l.right-max(size, l.height())*2
			if sameLine {
				// A gap between characters reads as a space unless either side is CJK.
				gap := position.Left-l.right > max(size, l.height())*0.2 && last < wideRune && r < wideRune
				if (space || gap) && last != ' ' {
					text = append(text, ' ')
				}
				text = append(text, char.Text...)
				last, space = lastRune(char.Text), false
				weights[size]++
				l.right, l.top, l.bottom = max(l.right, position.Right), max(l.top, position.Top), min(l.bottom, position.Bottom)
				continue
			}
		}
		finish()
		lines = append(lines, line{page: page, left: position.Left, right: position.Right, top: position.Top, bottom: position.Bottom})
		text = append(text[:0:0], char.Text...)
		last, space = lastRune(char.Text), false
		weights[size]++
	}
	finish()
	return lines
}

// lastRune returns the last rune of s.
func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
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
