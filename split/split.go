package split

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Options configures a Splitter. Lengths count code points with CRLF and lone
// CR counted as one line feed; each byte of an invalid UTF-8 sequence counts as one.
type Options struct {
	// Size is the target chunk length. It must be positive.
	Size int
	// MaxSize is the hard upper bound on chunk length. Zero means Size + Size/5.
	// A non-zero value must be at least Size.
	MaxSize int
	// Overlap is the maximum overlap between neighbouring chunks, from zero up to Size-1.
	Overlap int
}

// Splitter splits Markdown into chunks. It is immutable and safe for concurrent use.
type Splitter struct {
	size, maxSize, overlap int
}

// New validates opts and returns a Splitter.
func New(opts Options) (*Splitter, error) {
	switch {
	case opts.Size <= 0:
		return nil, errors.New("split: Size must be positive")
	case opts.MaxSize < 0:
		return nil, errors.New("split: MaxSize must not be negative")
	case opts.Overlap < 0 || opts.Overlap >= opts.Size:
		return nil, fmt.Errorf("split: Overlap must be in [0, %d)", opts.Size)
	}
	maxSize := opts.MaxSize
	if maxSize == 0 {
		maxSize = opts.Size + min(opts.Size/5, math.MaxInt-opts.Size)
	}
	if maxSize < opts.Size {
		return nil, errors.New("split: MaxSize must be at least Size")
	}
	return &Splitter{size: opts.Size, maxSize: maxSize, overlap: opts.Overlap}, nil
}

// Chunk is a contiguous byte range of the input.
type Chunk struct {
	// Text is input[Start:End] and shares memory with the input.
	Text string
	// Start and End are byte offsets into the input.
	Start, End int
	// Length is the length of Text as measured by Options.
	Length int
	// Headings is the inherited heading path, outermost first. It excludes a
	// heading that starts at or contains the chunk's first non-blank byte.
	Headings []Heading
	// TableHeader is set when the chunk's first non-blank byte is inside a table body.
	TableHeader *TableHeader
}

// Heading is a heading a chunk inherits.
type Heading struct {
	// Level is 1 to 6.
	Level int
	// Text is the heading's inline Markdown without block markers.
	Text string
	// Start and End delimit the heading block in the input, excluding its line terminator.
	Start, End int
}

// TableHeader is the header and delimiter rows of the table a chunk starts in.
type TableHeader struct {
	// Text is the two rows as they appear in the input.
	Text string
	// Start and End delimit the rows in the input, excluding the final line terminator.
	Start, End int
}

// Context renders the heading texts joined by " > " and the table header,
// separated by a newline. An empty part adds no separator.
// Its length is not bounded by Options: a long heading or a wide table header
// appears in full in every chunk under it.
func (c Chunk) Context() string {
	parts := make([]string, 0, 2)
	if len(c.Headings) > 0 {
		texts := make([]string, len(c.Headings))
		for i, heading := range c.Headings {
			texts[i] = heading.Text
		}
		parts = append(parts, strings.Join(texts, " > "))
	}
	if c.TableHeader != nil {
		parts = append(parts, c.TableHeader.Text)
	}
	return strings.Join(parts, "\n")
}

// TextWithContext returns Context, a blank line and Text, or Text alone when Context is empty.
func (c Chunk) TextWithContext() string {
	context := c.Context()
	if context == "" {
		return c.Text
	}
	return context + "\n\n" + c.Text
}

// Split splits text into chunks. Whitespace-only text yields nil.
// It panics if text is longer than math.MaxInt32 bytes.
func (s *Splitter) Split(text string) []Chunk {
	if strings.TrimFunc(text, unicode.IsSpace) == "" {
		return nil
	}
	l := scan(text)
	b := builder{layout: l}
	var chunks []Chunk
	prevEnd := 0
	for start := 0; ; {
		// Leading whitespace before a heading joins the heading's chunk.
		hard := l.nextHardStart(start)
		for hard < len(text) && l.contentEndBefore(hard) <= start {
			hard = l.nextHardStart(hard)
		}
		end := hard
		if l.length(start, hard) > s.maxSize {
			end = s.cut(l, start, hard, prevEnd)
		}
		chunks = append(chunks, b.chunk(start, end))
		if end == len(text) {
			return chunks
		}
		next := end
		if !l.isHardStart(end) {
			next = s.overlapStart(l, start, end)
		}
		prevEnd, start = end, next
	}
}

// cut chooses where a chunk starting at start ends, given that the text up to hard is too long.
func (s *Splitter) cut(l *layout, start, hard, prevEnd int) int {
	base := int(l.cp[start])
	span := int(l.cp[hard]) - base
	minimum := max(s.overlap+1, s.size/2)
	slack := s.maxSize - s.size
	contentEnd := max(l.contentEndBefore(hard), start)
	// A cut must leave content on both sides: after the first non-space rune and before the last.
	// The first content is only looked for within MaxSize; beyond it no cut can include it.
	window := l.lastPositionAt(base + s.maxSize)
	contentStart := start
	for contentStart < contentEnd && contentStart <= window {
		r, size := utf8.DecodeRuneInString(l.text[contentStart:])
		if !unicode.IsSpace(r) {
			break
		}
		contentStart += size
	}
	// Trailing whitespace shorter than MaxSize can join the last content, so cuts stay before it;
	// longer trailing whitespace needs a chunk of its own anyway.
	tailAlone := l.length(contentEnd, hard) >= s.maxSize
	usable := func(i int) bool { return i > contentStart && (i < contentEnd || tailAlone) }

	// Spread the text to the next heading over even pieces when a block boundary lies within reach;
	// otherwise spread the current block.
	lo := l.positionAt(base + minimum)
	remaining, upper := int(l.cp[l.nextBlock(lo)])-base, s.size
	if remaining <= s.maxSize {
		remaining, upper = span, s.maxSize
	}
	pieces := ceilDiv(remaining-s.overlap, s.size-s.overlap)
	target := ceilDiv(remaining+(pieces-1)*s.overlap, pieces)

	// pick returns the highest-ranked usable boundary in [from, to] at or above floor, nearest the target.
	// When strict, boundaries beyond Size must be block boundaries and none may leave a tail within the
	// slack before the next heading.
	pick := func(from, to int, floor rank, strict bool) (int, rank) {
		best, bestRank, bestDistance := -1, floor-1, 0
		for i := from; i <= to; i++ {
			value := l.ranks[i]
			length := int(l.cp[i]) - base
			if value < floor || !usable(i) ||
				strict && (length > s.size && value != rankBlock || int(l.cp[hard])-int(l.cp[i]) <= slack) {
				continue
			}
			distance := abs(length - target)
			if value > bestRank || value == bestRank && distance < bestDistance {
				best, bestRank, bestDistance = i, value, distance
			}
		}
		return best, bestRank
	}
	best, bestRank := pick(lo, l.lastPositionAt(base+upper), rankEnclosed, true)
	if bestRank > rankEnclosed {
		return best
	}
	// Keep a line or sentence whole when it still fits in MaxSize.
	if wider, _ := pick(lo, l.lastPositionAt(base+s.maxSize), rankSpace, false); wider >= 0 {
		return wider
	}
	// Keep a heading whole when it fits but its body's first line does not.
	if i := sort.SearchInts(l.headingEnds, window+1) - 1; i >= 0 && l.headingEnds[i] > max(start, prevEnd) && usable(l.headingEnds[i]) {
		return l.headingEnds[i]
	}
	// Otherwise cut at the last line boundary before the minimum.
	for i := lo - 1; i > max(start, prevEnd); i-- {
		if l.ranks[i] >= rankLine && usable(i) {
			return i
		}
	}
	if best >= 0 {
		return best
	}
	// No boundary at all: cut at the target, kept within the content when possible.
	position := l.positionAt(base + target)
	if position >= contentEnd && contentStart < contentEnd && !tailAlone {
		// Leave the last rune of content to the next chunk so it is not whitespace only.
		_, size := utf8.DecodeLastRuneInString(l.text[:contentEnd])
		position = contentEnd - size
		if position <= max(start, prevEnd, contentStart) {
			position = contentEnd
		}
	}
	if after := int(l.cp[contentStart]) + 1; position <= contentStart && contentStart < contentEnd && after-base <= s.maxSize {
		position = l.positionAt(after)
	}
	// Chunks must advance past the previous end.
	if floor := max(start, prevEnd); position <= floor {
		position = l.positionAt(int(l.cp[floor]) + 1)
	}
	// Keep a grapheme cluster whole when one of its ends lies within reach.
	if l.ranks[position] == rankJoined {
		for i := position - 1; i > max(start, prevEnd, contentStart); i-- {
			if l.ranks[i] >= rankNone {
				return i
			}
		}
		for i, limit := position+1, l.lastPositionAt(base+s.maxSize); i <= limit && (i < contentEnd || tailAlone); i++ {
			if l.ranks[i] >= rankNone {
				return i
			}
		}
	}
	return position
}

// overlapStart returns where the chunk after [start, end) begins: the highest-ranked, earliest
// boundary inside the overlap window, or end when the window offers no boundary.
func (s *Splitter) overlapStart(l *layout, start, end int) int {
	lo := l.positionAt(int(l.cp[end]) - s.overlap)
	if lo <= start {
		return end
	}
	tail := end
	for tail > lo {
		r, size := utf8.DecodeLastRuneInString(l.text[:tail])
		if !unicode.IsSpace(r) {
			break
		}
		tail -= size
	}
	best, bestRank := end, rankEnclosed
	for i := lo; i < tail; i++ {
		if l.ranks[i] > bestRank {
			best, bestRank = i, l.ranks[i]
		}
	}
	return best
}

// builder turns byte ranges into chunks, tracking the heading path as chunks advance.
type builder struct {
	*layout
	stack       []headingBlock
	nextHeading int
	table       int
}

// chunk builds the chunk for [start, end).
func (b *builder) chunk(start, end int) Chunk {
	anchor := start
	for anchor < end {
		r, size := utf8.DecodeRuneInString(b.text[anchor:])
		if !unicode.IsSpace(r) {
			break
		}
		anchor += size
	}
	for b.nextHeading < len(b.headings) && b.headings[b.nextHeading].start < anchor {
		item := b.headings[b.nextHeading]
		for len(b.stack) > 0 && b.stack[len(b.stack)-1].level >= item.level {
			b.stack = b.stack[:len(b.stack)-1]
		}
		b.stack = append(b.stack, item)
		b.nextHeading++
	}
	// Leave out a heading that starts at or contains the anchor, together with its siblings and descendants.
	path := b.stack
	if n := len(path); n > 0 && path[n-1].end > anchor {
		path = path[:n-1]
	}
	if b.nextHeading < len(b.headings) && b.headings[b.nextHeading].start == anchor {
		for len(path) > 0 && path[len(path)-1].level >= b.headings[b.nextHeading].level {
			path = path[:len(path)-1]
		}
	}
	c := Chunk{Text: b.text[start:end], Start: start, End: end, Length: b.length(start, end)}
	if len(path) > 0 {
		c.Headings = make([]Heading, len(path))
		for i, item := range path {
			c.Headings[i] = Heading{Level: item.level, Text: item.text, Start: item.start, End: item.end}
		}
	}
	for b.table < len(b.tables) && b.tables[b.table].end <= anchor {
		b.table++
	}
	if b.table < len(b.tables) {
		if table := b.tables[b.table]; table.hasHeader && table.dataStart <= anchor && anchor < table.end {
			c.TableHeader = &TableHeader{
				Text: b.text[table.headerStart:table.headerEnd], Start: table.headerStart, End: table.headerEnd,
			}
		}
	}
	return c
}

// length returns the measured length of text[start:end].
func (l *layout) length(start, end int) int {
	return int(l.cp[end] - l.cp[start])
}

// positionAt returns the first valid position whose code point index is at least c.
func (l *layout) positionAt(c int) int {
	i := sort.Search(len(l.cp), func(i int) bool { return int(l.cp[i]) >= c })
	for i < len(l.text) && l.ranks[i] == rankInvalid {
		i++
	}
	return i
}

// lastPositionAt returns the last valid position whose code point index is at most c.
func (l *layout) lastPositionAt(c int) int {
	i := sort.Search(len(l.cp), func(i int) bool { return int(l.cp[i]) > c }) - 1
	for i > 0 && l.ranks[i] == rankInvalid {
		i--
	}
	return i
}

// nextHardStart returns the first standalone heading start after position, or the input length.
func (l *layout) nextHardStart(position int) int {
	i := sort.SearchInts(l.hardStarts, position+1)
	if i == len(l.hardStarts) {
		return len(l.text)
	}
	return l.hardStarts[i]
}

// isHardStart reports whether position starts a standalone heading.
func (l *layout) isHardStart(position int) bool {
	i := sort.SearchInts(l.hardStarts, position)
	return i < len(l.hardStarts) && l.hardStarts[i] == position
}

// nextBlock returns the first block boundary at or after position, or the input length.
func (l *layout) nextBlock(position int) int {
	i := sort.SearchInts(l.blocks, position)
	if i == len(l.blocks) {
		return len(l.text)
	}
	return l.blocks[i]
}

// contentEndBefore returns the position after the last non-whitespace rune before hard, or 0.
// Results are cached per hard position, so long trailing whitespace is scanned once.
func (l *layout) contentEndBefore(hard int) int {
	if end, ok := l.contentEnds[hard]; ok {
		return end
	}
	end := lastContentEnd(l.text, 0, hard)
	if l.contentEnds == nil {
		l.contentEnds = make(map[int]int)
	}
	l.contentEnds[hard] = end
	return end
}

// lastContentEnd returns the position after the last non-whitespace rune in text[start:end].
func lastContentEnd(text string, start, end int) int {
	for end > start {
		r, size := utf8.DecodeLastRuneInString(text[start:end])
		if !unicode.IsSpace(r) {
			break
		}
		end -= size
	}
	return end
}

// ceilDiv divides rounding up; both operands are positive.
func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}

// abs returns the absolute value of n.
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
