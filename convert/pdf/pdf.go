// Package pdf converts PDF documents to Markdown with PDFium compiled to
// WebAssembly, so it needs no cgo or system libraries.
//
// Each page becomes a convert.KindPage section. Text lines are grouped by
// font size: the size with the most text is body text, and larger short
// lines become headings unless Options.NoHeadings is set. Paragraphs break
// where line spacing grows or the font size changes. Reading order follows
// PDFium's text order; multi-column layout and table recovery are not
// attempted. A PDF without a text layer yields empty Markdown and one empty
// section per page.
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
	pool     pdfium.Pool
	inFlight sync.WaitGroup
}

// New returns a PDF converter. PDFium starts on first use.
func New(opts Options) *Converter {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	return &Converter{opts: opts, slots: make(chan struct{}, opts.Workers), done: make(chan struct{})}
}

// Close releases the PDFium instances after conversions in flight finish.
// Calls waiting for a worker, and later calls, return convert.ErrClosed.
// Close is idempotent.
func (c *Converter) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.done)
	c.mu.Unlock()
	c.inFlight.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool == nil {
		return nil
	}
	err := c.pool.Close()
	c.pool = nil
	return err
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
	return render(lines, pages, !c.opts.NoHeadings, limits.MaxOutputBytes)
}

// line is a line of text on a page in PDF points, y axis pointing up.
type line struct {
	page                     int
	text                     string
	size                     float64
	left, right, top, bottom float64
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
			Mode:                   requests.GetPageTextStructuredModeRects,
			CollectFontInformation: true,
		})
		if err != nil {
			return nil, 0, fmt.Errorf("%w: page %d: %w", convert.ErrCorrupt, index+1, err)
		}
		// Text blocks on one line merge; the line takes the font size of its longest block.
		dominant := 0
		for _, rect := range page.Rects {
			text := strings.TrimRight(rect.Text, "\r\n")
			if strings.TrimSpace(text) == "" {
				continue
			}
			size := 0.0
			if rect.FontInformation != nil {
				size = rect.FontInformation.RenderedSize
			}
			position := rect.PointPosition
			current := len(lines) - 1
			if current < 0 || lines[current].page != index || position.Top < lines[current].bottom ||
				position.Bottom > lines[current].top || position.Left < lines[current].left {
				lines = append(lines, line{page: index, text: text, size: size,
					left: position.Left, right: position.Right, top: position.Top, bottom: position.Bottom})
				dominant = utf8.RuneCountInString(text)
				continue
			}
			l := &lines[current]
			last, _ := utf8.DecodeLastRuneInString(l.text)
			first, _ := utf8.DecodeRuneInString(text)
			// Separated blocks get a space unless either side is CJK or already a space.
			if position.Left-l.right > max(size, l.size)*0.2 && !unicode.IsSpace(last) && !unicode.IsSpace(first) &&
				last < wideRune && first < wideRune {
				l.text += " "
			}
			l.text += text
			if runes := utf8.RuneCountInString(text); runes > dominant {
				l.size, dominant = size, runes
			}
			l.right, l.top, l.bottom = max(l.right, position.Right), max(l.top, position.Top), min(l.bottom, position.Bottom)
		}
	}
	return lines, count.PageCount, nil
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
