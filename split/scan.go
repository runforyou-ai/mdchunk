package split

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// rank is the quality of cutting before a byte position; higher is better.
type rank int8

const (
	rankInvalid  rank = iota - 1 // inside a UTF-8 sequence or a CRLF; never cut here
	rankNone                     // allowed only as a last resort
	rankEnclosed                 // punctuation or space inside code, tables, HTML or code spans
	rankSpace
	rankClause
	rankSentence
	rankLine
	rankBlock
)

// lineKind is the structural role of a line.
type lineKind uint8

const (
	kindBlank lineKind = iota
	kindText
	kindHeading // ATX heading
	kindSetext  // setext underline
	kindBreak   // thematic break
	kindFence   // opening or closing code fence
	kindCode    // fenced code content
	kindIndented
	kindTable
	kindListItem
	kindListCont
	kindQuote
	kindHTML
	kindFrontMatter
)

// enclosed reports whether the kind only offers line breaks as boundaries.
func (k lineKind) enclosed() bool {
	switch k {
	case kindFence, kindCode, kindIndented, kindTable, kindHTML, kindFrontMatter:
		return true
	}
	return false
}

// line is one input line; [start, end) excludes the line terminator.
type line struct {
	start, end int
	kind       lineKind
	block      int // structural block id; -1 for blank lines
}

// headingBlock is an ATX heading line or a setext heading paragraph with its underline.
type headingBlock struct {
	start, end int
	level      int
	text       string
	attached   bool // directly follows another heading, ignoring blank lines
}

// tableBlock is a GFM table; the header and delimiter rows span [headerStart, headerEnd).
type tableBlock struct {
	headerStart, headerEnd int
	dataStart, end         int
	hasHeader              bool
}

// layout holds everything the splitter needs to know about one input.
type layout struct {
	text       string
	ranks      []rank  // per byte position, len(text)+1
	cp         []int32 // code points before each byte position, CRLF counted once
	headings   []headingBlock
	tables     []tableBlock
	hardStarts []int // starts of headings that are not attached, ascending
	blocks     []int // positions ranked rankBlock, ascending
	// headingEnds are the line starts right after heading blocks, ascending; they are cut only
	// when a heading's body does not fit.
	headingEnds []int

	contentEnds map[int]int // cache for contentEndBefore
}

// htmlEnd is the end condition of an open HTML block.
type htmlEnd uint8

const (
	htmlNone htmlEnd = iota
	htmlClosingTag
	htmlComment
	htmlBlankLine
)

var (
	htmlRawTags   = []string{"script", "pre", "style", "textarea"}
	htmlBlockTags = map[string]bool{
		"address": true, "article": true, "aside": true, "base": true, "basefont": true, "blockquote": true,
		"body": true, "caption": true, "center": true, "col": true, "colgroup": true, "dd": true, "details": true,
		"dialog": true, "dir": true, "div": true, "dl": true, "dt": true, "fieldset": true, "figcaption": true,
		"figure": true, "footer": true, "form": true, "frame": true, "frameset": true, "h1": true, "h2": true,
		"h3": true, "h4": true, "h5": true, "h6": true, "head": true, "header": true, "hr": true, "html": true,
		"iframe": true, "legend": true, "li": true, "link": true, "main": true, "menu": true, "menuitem": true,
		"nav": true, "noframes": true, "ol": true, "optgroup": true, "option": true, "p": true, "param": true,
		"search": true, "section": true, "summary": true, "table": true, "tbody": true, "td": true, "tfoot": true,
		"th": true, "thead": true, "title": true, "tr": true, "track": true, "ul": true,
	}
)

// scan measures text and classifies its Markdown structure.
func scan(text string) *layout {
	if len(text) > math.MaxInt32 {
		panic("split: input longer than math.MaxInt32 bytes")
	}
	l := &layout{text: text, ranks: make([]rank, len(text)+1), cp: make([]int32, len(text)+1)}
	l.measure()
	lines := splitLines(text)
	l.classify(lines)
	l.rankLines(lines)
	for position, value := range l.ranks {
		if value == rankBlock {
			l.blocks = append(l.blocks, position)
		}
	}
	for _, heading := range l.headings {
		if !heading.attached {
			l.hardStarts = append(l.hardStarts, heading.start)
		}
	}
	return l
}

// measure fills the code point index and marks positions that split a rune or a CRLF.
func (l *layout) measure() {
	text := l.text
	for i := 0; i < len(text); {
		count := l.cp[i]
		if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
			l.cp[i+1], l.ranks[i+1] = count, rankInvalid
			l.cp[i+2] = count + 1
			i += 2
			continue
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		for k := 1; k < size; k++ {
			l.cp[i+k], l.ranks[i+k] = count, rankInvalid
		}
		l.cp[i+size] = count + 1
		i += size
	}
}

// splitLines splits text at LF, CRLF and lone CR. A trailing terminator yields an empty last line.
func splitLines(text string) []line {
	var lines []line
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			lines = append(lines, line{start: start, end: i})
			start = i + 1
		case '\r':
			lines = append(lines, line{start: start, end: i})
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	return append(lines, line{start: start, end: len(text)})
}

// classify assigns a kind and block id to every line and records headings and tables.
func (l *layout) classify(lines []line) {
	var (
		fenceChar      byte
		fenceLen       int
		inFence        bool
		html           htmlEnd
		inList         bool
		tableOpen      bool
		frontMatterEnd = l.frontMatterEnd(lines)
		blockID        = -1
		paraStart      = -1 // first line of the current paragraph
		lastNonBlank   = -1 // index of the previous non-blank line
		prevKind       = kindBlank
	)
	newBlock := func() int {
		blockID++
		return blockID
	}
	for i := range lines {
		ln := &lines[i]
		content := l.text[ln.start:ln.end]
		if i == 0 {
			content = strings.TrimPrefix(content, "\uFEFF")
		}
		indent, rest := indentation(content)
		blank := strings.TrimLeft(content, " \t") == ""
		sameAsPrev := func() int {
			if lastNonBlank < 0 {
				return newBlock()
			}
			return lines[lastNonBlank].block
		}

		switch {
		case i <= frontMatterEnd:
			ln.kind = kindFrontMatter
			if i == 0 {
				ln.block = newBlock()
			} else {
				ln.block = lines[0].block
			}
		case inFence:
			ln.kind, ln.block = kindCode, sameAsPrevLine(lines, i)
			if indent <= 3 {
				if run := leadingRun(rest, fenceChar); run >= fenceLen && isBlank(rest[run:]) {
					ln.kind, inFence = kindFence, false
				}
			}
		case html != htmlNone && (html != htmlBlankLine || !blank):
			ln.kind, ln.block = kindHTML, sameAsPrevLine(lines, i)
			if htmlEnds(html, content) {
				html = htmlNone
			}
		case tableOpen && !blank && hasUnescapedPipe(rest):
			ln.kind, ln.block = kindTable, sameAsPrevLine(lines, i)
			l.tables[len(l.tables)-1].end = ln.end
		default:
			html, tableOpen = htmlNone, false
			ln.kind, ln.block = l.classifyFresh(lines, i, indent, rest, blank, inList, prevKind, paraStart, lastNonBlank, newBlock, sameAsPrev)
			switch ln.kind {
			case kindFence:
				inFence, fenceChar, fenceLen = true, rest[0], leadingRun(rest, rest[0])
			case kindHTML:
				html = htmlStart(rest)
				if htmlEnds(html, rest[1:]) && html != htmlBlankLine {
					html = htmlNone
				}
			case kindTable:
				tableOpen = true
			case kindListItem:
				inList = true
			case kindText:
				if indent == 0 {
					inList = false
				}
			case kindHeading, kindSetext, kindBreak, kindQuote, kindIndented:
				if indent == 0 {
					inList = false
				}
			}
		}

		if ln.kind == kindBlank {
			ln.block = -1
		}
		if ln.kind == kindText {
			if prevKind != kindText {
				paraStart = i
			}
		} else {
			paraStart = -1
		}
		if ln.kind != kindBlank {
			lastNonBlank = i
		}
		prevKind = ln.kind
	}
}

// classifyFresh classifies a line that does not continue a fence, HTML block, table or front matter.
func (l *layout) classifyFresh(lines []line, i, indent int, rest string, blank, inList bool, prevKind lineKind,
	paraStart, lastNonBlank int, newBlock, sameAsPrev func() int) (lineKind, int) {
	ln := &lines[i]
	block := indent <= 3
	switch {
	case blank:
		return kindBlank, -1
	case block && isFenceOpen(rest):
		return kindFence, newBlock()
	case block && htmlStart(rest) != htmlNone:
		return kindHTML, newBlock()
	case block && prevKind == kindText && isSetextUnderline(rest):
		level := 1
		if rest[0] == '-' {
			level = 2
		}
		id := newBlock()
		parts := make([]string, 0, i-paraStart)
		for j := paraStart; j < i; j++ {
			lines[j].kind, lines[j].block = kindHeading, id
			parts = append(parts, strings.TrimSpace(l.text[lines[j].start:lines[j].end]))
		}
		previous := paraStart - 1
		for previous >= 0 && lines[previous].kind == kindBlank {
			previous--
		}
		l.addHeading(lines, paraStart, i, level, strings.Join(parts, " "), previous)
		return kindSetext, id
	case block && isThematicBreak(rest):
		return kindBreak, newBlock()
	case block && isListItem(rest) && (prevKind != kindText || interruptsParagraph(rest)):
		return kindListItem, newBlock()
	case inList && indent >= 1:
		return kindListCont, sameAsPrev()
	case block && atxLevel(rest) > 0:
		level := atxLevel(rest)
		l.addHeading(lines, i, i, level, atxText(rest[level:]), lastNonBlank)
		return kindHeading, newBlock()
	case block && prevKind == kindText && isDelimiterRow(rest):
		header := l.text[lines[i-1].start:lines[i-1].end]
		headerCells := tableCells(header)
		if hasUnescapedPipe(header) && len(headerCells) == len(tableCells(rest)) {
			id := newBlock()
			lines[i-1].kind, lines[i-1].block = kindTable, id
			table := tableBlock{headerStart: lines[i-1].start, headerEnd: ln.end, dataStart: ln.end, end: ln.end}
			if i+1 < len(lines) {
				table.dataStart = lines[i+1].start
			}
			table.hasHeader = slices.ContainsFunc(headerCells, func(cell string) bool { return cell != "" })
			l.tables = append(l.tables, table)
			return kindTable, id
		}
	case block && rest[0] == '>':
		if prevKind == kindQuote {
			return kindQuote, sameAsPrev()
		}
		return kindQuote, newBlock()
	case indent >= 4 && !inList && prevKind != kindText && prevKind != kindQuote && prevKind != kindListCont:
		if lastNonBlank >= 0 && lines[lastNonBlank].kind == kindIndented {
			return kindIndented, sameAsPrev()
		}
		return kindIndented, newBlock()
	}
	if inList && prevKind != kindBlank {
		return kindListCont, sameAsPrev()
	}
	if prevKind == kindText {
		return kindText, sameAsPrev()
	}
	return kindText, newBlock()
}

// addHeading records a heading spanning lines first..last; previous is the last non-blank line before it.
func (l *layout) addHeading(lines []line, first, last, level int, text string, previous int) {
	attached := previous >= 0 && (lines[previous].kind == kindHeading || lines[previous].kind == kindSetext)
	l.headings = append(l.headings, headingBlock{
		start: lines[first].start, end: lines[last].end, level: level, text: text, attached: attached,
	})
}

// sameAsPrevLine returns the block id of the line before i.
func sameAsPrevLine(lines []line, i int) int {
	return lines[i-1].block
}

// frontMatterEnd returns the index of the closing front matter line, or -1.
func (l *layout) frontMatterEnd(lines []line) int {
	first := strings.TrimPrefix(l.text[lines[0].start:lines[0].end], "\uFEFF")
	if strings.TrimRight(first, " \t") != "---" {
		return -1
	}
	for i := 1; i < len(lines); i++ {
		if closing := strings.TrimRight(l.text[lines[i].start:lines[i].end], " \t"); closing == "---" || closing == "..." {
			return i
		}
	}
	return -1
}

// rankLines ranks line starts and positions within lines.
func (l *layout) rankLines(lines []line) {
	headingLine := make([]bool, len(lines))
	for i := range lines {
		headingLine[i] = lines[i].kind == kindHeading || lines[i].kind == kindSetext
	}
	lastNonBlank := -1
	for i := range lines {
		ln := lines[i]
		if i > 0 {
			value := rankLine
			if ln.kind != kindBlank && lastNonBlank >= 0 {
				prev := lines[lastNonBlank]
				switch {
				case ln.block != prev.block:
					value = rankBlock
				case lines[i-1].kind == kindBlank && !ln.kind.enclosed():
					value = rankBlock
				case ln.kind == kindText && lines[i-1].kind == kindText && softWrap(l.text[lines[i-1].start:lines[i-1].end], l.text[ln.start:ln.end]):
					value = rankSpace
				}
			}
			if l.ranks[ln.start] != rankInvalid {
				l.ranks[ln.start] = value
			}
		}
		l.rankWithin(ln, headingLine[i])
		if ln.kind != kindBlank {
			lastNonBlank = i
		}
	}
	// A heading stays with the start of its body: no cut between them or inside a heading block.
	for i := range lines {
		if !headingLine[i] {
			continue
		}
		if i+1 < len(lines) && !headingLine[i+1] {
			l.headingEnds = append(l.headingEnds, lines[i+1].start)
		}
		for j := i + 1; j < len(lines); j++ {
			l.ranks[lines[j].start] = rankNone
			if lines[j].kind != kindBlank {
				break
			}
		}
	}
	// Standalone headings are block boundaries; a heading run is cut between its headings only when too long.
	for _, heading := range l.headings {
		if heading.attached {
			l.ranks[heading.start] = rankLine
		} else {
			l.ranks[heading.start] = rankBlock
		}
	}
}

// rankWithin ranks the positions after each rune of a line.
func (l *layout) rankWithin(ln line, heading bool) {
	text := l.text
	enclosed := ln.kind.enclosed()
	var spans [][2]int
	if !enclosed && !heading {
		spans = codeSpans(text[ln.start:ln.end])
	}
	span := 0
	for position := ln.start + 1; position <= ln.end; position++ {
		if l.ranks[position] == rankInvalid {
			continue
		}
		value := rankNone
		if !heading {
			value = punctuationRank(text[ln.start:position])
		}
		// Spans are sorted and positions increase, so the current span only moves forward.
		offset := position - ln.start
		for span < len(spans) && offset >= spans[span][1] {
			span++
		}
		inside := span < len(spans) && offset > spans[span][0]
		if value > rankNone && (enclosed || inside) {
			value = rankEnclosed
		}
		l.ranks[position] = value
	}
}

// punctuationRank ranks a cut after the last rune of prefix.
func punctuationRank(prefix string) rank {
	last, size := utf8.DecodeLastRuneInString(prefix)
	switch last {
	case '。', '！', '？':
		return rankSentence
	case '，', '；':
		return rankClause
	case ' ', '\t':
		before, _ := utf8.DecodeLastRuneInString(prefix[:len(prefix)-size])
		switch before {
		case '.', '!', '?':
			return rankSentence
		case ',', ';':
			return rankClause
		}
		return rankSpace
	}
	return rankNone
}

// softWrap reports whether the break between two adjacent text lines only wraps a sentence.
func softWrap(previous, current string) bool {
	if strings.HasSuffix(previous, "  ") || strings.HasSuffix(previous, "\\") {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(strings.TrimRight(previous, " \t"))
	first, _ := utf8.DecodeRuneInString(strings.TrimLeft(current, " \t"))
	if !unicode.IsLetter(last) && !unicode.IsNumber(last) {
		return false
	}
	return unicode.IsLetter(first) || unicode.IsNumber(first) || strings.ContainsRune("，。、；：！？）」』》,.;:!?)", first)
}

// codeSpans returns the byte ranges of matched backtick code spans of a line, ascending.
// A run opens a span closed by the next run of the same length; runs inside a span are content.
func codeSpans(content string) [][2]int {
	type run struct{ start, length int }
	var runs []run
	byLength := map[int][]int{} // run indexes per length, ascending
	for i := 0; i < len(content); {
		if content[i] != '`' {
			i++
			continue
		}
		length := leadingRun(content[i:], '`')
		byLength[length] = append(byLength[length], len(runs))
		runs = append(runs, run{i, length})
		i += length
	}
	var spans [][2]int
	next := map[int]int{} // per length, the first candidate in byLength not yet passed
	for i := 0; i < len(runs); i++ {
		candidates := byLength[runs[i].length]
		k := next[runs[i].length]
		for k < len(candidates) && candidates[k] <= i {
			k++
		}
		next[runs[i].length] = k
		if k == len(candidates) {
			continue
		}
		closing := runs[candidates[k]]
		spans = append(spans, [2]int{runs[i].start, closing.start + closing.length})
		i = candidates[k]
	}
	return spans
}

// indentation returns the indentation width (tabs to the next multiple of 4) and the rest of the line.
func indentation(content string) (int, string) {
	width := 0
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case ' ':
			width++
		case '\t':
			width += 4 - width%4
		default:
			return width, content[i:]
		}
	}
	return width, ""
}

// isBlank reports whether s holds only spaces and tabs.
func isBlank(s string) bool {
	return strings.TrimLeft(s, " \t") == ""
}

// leadingRun counts leading bytes equal to c.
func leadingRun(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// isFenceOpen reports whether rest opens a fenced code block.
func isFenceOpen(rest string) bool {
	if rest == "" || rest[0] != '`' && rest[0] != '~' {
		return false
	}
	run := leadingRun(rest, rest[0])
	return run >= 3 && (rest[0] == '~' || !strings.Contains(rest[run:], "`"))
}

// atxLevel returns the level of an ATX heading, or 0.
func atxLevel(rest string) int {
	level := leadingRun(rest, '#')
	if level < 1 || level > 6 {
		return 0
	}
	if level < len(rest) && rest[level] != ' ' && rest[level] != '\t' {
		return 0
	}
	return level
}

// atxText returns heading content without the optional closing sequence.
func atxText(after string) string {
	text := strings.TrimSpace(after)
	trimmed := strings.TrimRight(text, "#")
	switch {
	case trimmed == "":
		return ""
	case len(trimmed) < len(text) && (strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t")):
		return strings.TrimSpace(trimmed)
	}
	return text
}

// isSetextUnderline reports whether rest is a run of '=' or '-' with optional trailing spaces.
func isSetextUnderline(rest string) bool {
	trimmed := strings.TrimRight(rest, " \t")
	if trimmed == "" || trimmed[0] != '=' && trimmed[0] != '-' {
		return false
	}
	return leadingRun(trimmed, trimmed[0]) == len(trimmed)
}

// isThematicBreak reports whether rest is three or more '*', '-' or '_' with optional spaces.
func isThematicBreak(rest string) bool {
	if rest == "" || !strings.ContainsRune("*-_", rune(rest[0])) {
		return false
	}
	count := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case rest[0]:
			count++
		case ' ', '\t':
		default:
			return false
		}
	}
	return count >= 3
}

// isListItem reports whether rest starts with a bullet or ordered list marker.
func isListItem(rest string) bool {
	marker := 0
	switch {
	case rest == "":
		return false
	case strings.ContainsRune("-*+", rune(rest[0])):
		marker = 1
	default:
		digits := 0
		for digits < len(rest) && digits < 10 && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits == 0 || digits > 9 || digits == len(rest) || rest[digits] != '.' && rest[digits] != ')' {
			return false
		}
		marker = digits + 1
	}
	return marker == len(rest) || rest[marker] == ' ' || rest[marker] == '\t'
}

// interruptsParagraph reports whether a list item may interrupt a paragraph:
// it has content and, when ordered, starts at 1.
func interruptsParagraph(rest string) bool {
	marker := strings.IndexAny(rest, " \t")
	if marker < 0 || isBlank(rest[marker:]) {
		return false
	}
	if strings.ContainsRune("-*+", rune(rest[0])) {
		return true
	}
	number, err := strconv.Atoi(rest[:marker-1])
	return err == nil && number == 1
}

// htmlStart returns the end condition of an HTML block starting at rest.
func htmlStart(rest string) htmlEnd {
	if !strings.HasPrefix(rest, "<") {
		return htmlNone
	}
	if strings.HasPrefix(rest, "<!--") {
		return htmlComment
	}
	lower := strings.ToLower(rest)
	for _, tag := range htmlRawTags {
		if after, ok := strings.CutPrefix(lower, "<"+tag); ok && (after == "" || strings.ContainsRune(" \t>", rune(after[0]))) {
			return htmlClosingTag
		}
	}
	name := strings.TrimPrefix(lower[1:], "/")
	end := 0
	for end < len(name) && (name[end] >= 'a' && name[end] <= 'z' || name[end] >= '0' && name[end] <= '9') {
		end++
	}
	if !htmlBlockTags[name[:end]] {
		return htmlNone
	}
	after := name[end:]
	if after == "" || strings.ContainsRune(" \t>", rune(after[0])) || strings.HasPrefix(after, "/>") {
		return htmlBlankLine
	}
	return htmlNone
}

// htmlEnds reports whether content satisfies the end condition of an open HTML block.
func htmlEnds(end htmlEnd, content string) bool {
	switch end {
	case htmlComment:
		return strings.Contains(content, "-->")
	case htmlClosingTag:
		lower := strings.ToLower(content)
		for _, tag := range htmlRawTags {
			if strings.Contains(lower, "</"+tag+">") {
				return true
			}
		}
	}
	return false
}

// hasUnescapedPipe reports whether s contains a '|' not directly preceded by a
// backslash; as in GFM, the backslash escapes the pipe whatever precedes it.
func hasUnescapedPipe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && (i == 0 || s[i-1] != '\\') {
			return true
		}
	}
	return false
}

// tableCells splits a table row into trimmed cells at pipes not directly
// preceded by a backslash, ignoring optional edge pipes.
func tableCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	if strings.HasSuffix(row, "|") && !strings.HasSuffix(row, "\\|") {
		row = row[:len(row)-1]
	}
	var cells []string
	start := 0
	for i := 0; i < len(row); i++ {
		if row[i] == '|' && (i == 0 || row[i-1] != '\\') {
			cells = append(cells, strings.TrimSpace(row[start:i]))
			start = i + 1
		}
	}
	return append(cells, strings.TrimSpace(row[start:]))
}

// isDelimiterRow reports whether rest is a GFM table delimiter row.
func isDelimiterRow(rest string) bool {
	if !strings.Contains(rest, "|") {
		return false
	}
	for _, cell := range tableCells(rest) {
		cell = strings.TrimSuffix(strings.TrimPrefix(cell, ":"), ":")
		if cell == "" || leadingRun(cell, '-') != len(cell) {
			return false
		}
	}
	return true
}
