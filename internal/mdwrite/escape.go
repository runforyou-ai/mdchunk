package mdwrite

import (
	"strings"

	"github.com/runforyou-ai/mdchunk/convert"
)

// lineEndings converts CRLF and lone CR to LF and drops NUL bytes.
var lineEndings = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\x00", "")

// Paragraph escapes source text so that none of its lines opens a Markdown
// block structure: line endings become LF, NUL bytes are dropped, indentation is removed and a
// line-leading marker is backslash-escaped.
func Paragraph(text string) string {
	lines := strings.Split(lineEndings.Replace(text), "\n")
	for i, line := range lines {
		lines[i] = escapeLine(strings.TrimLeft(line, " \t"))
	}
	return strings.Join(lines, "\n")
}

// escapeLine backslash-escapes a block marker at the start of line.
func escapeLine(line string) string {
	if line == "" {
		return line
	}
	switch line[0] {
	case '#', '>', '-', '+', '*', '=', '|', '`', '~', '<', '_':
		return `\` + line
	}
	digits := 0
	for digits < len(line) && digits < 10 && line[digits] >= '0' && line[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits < len(line) && (line[digits] == '.' || line[digits] == ')') {
		return line[:digits] + `\` + line[digits:]
	}
	return line
}

// cellEscapes escapes backslashes and pipes in table cells.
var cellEscapes = strings.NewReplacer(`\`, `\\`, "|", `\|`, "\x00", "")

// Cell collapses whitespace in a table cell, drops NUL bytes and escapes
// backslashes and pipes.
func Cell(text string) string {
	return cellEscapes.Replace(strings.Join(strings.Fields(text), " "))
}

// Table collects rows for a GFM table and renders it under an output limit.
// Rows whose cells are all empty are skipped.
type Table struct {
	header      []string
	hasHeader   bool
	firstHeader bool
	rows        [][]string
	width       int
	cellBytes   int64
	max         int64
}

// NewTable returns a Table whose rendering may not exceed maxBytes; maxBytes < 0
// means unlimited. With firstRowHeader the first added row is the header;
// otherwise the table renders under an empty header row unless SetHeader is called.
func NewTable(maxBytes int64, firstRowHeader bool) *Table {
	return &Table{max: maxBytes, firstHeader: firstRowHeader}
}

// SetHeader sets the header row, keeping it even when its cells are empty.
func (t *Table) SetHeader(row []string) error {
	for _, cell := range t.header {
		t.cellBytes -= int64(len(cell))
	}
	t.header, t.hasHeader, t.firstHeader = escapeCells(row), true, false
	t.width = max(t.width, len(t.header))
	for _, cell := range t.header {
		t.cellBytes += int64(len(cell))
	}
	return t.check()
}

// Add appends a row. It returns a *convert.LimitError once the rendered table
// would exceed its limit.
func (t *Table) Add(row []string) error {
	cells := escapeCells(row)
	blank := true
	for _, cell := range cells {
		blank = blank && cell == ""
	}
	if blank {
		return nil
	}
	if t.firstHeader && !t.hasHeader {
		t.header, t.hasHeader = cells, true
	} else {
		t.rows = append(t.rows, cells)
	}
	t.width = max(t.width, len(cells))
	for _, cell := range cells {
		t.cellBytes += int64(len(cell))
	}
	return t.check()
}

// escapeCells applies Cell to every cell of row.
func escapeCells(row []string) []string {
	cells := make([]string, len(row))
	for i, cell := range row {
		cells[i] = Cell(cell)
	}
	return cells
}

// Len returns the number of rows added, including a header.
func (t *Table) Len() int {
	if t.hasHeader {
		return len(t.rows) + 1
	}
	return len(t.rows)
}

// size returns the length of the rendered table.
func (t *Table) size() int64 {
	if !t.hasHeader && len(t.rows) == 0 {
		return 0
	}
	width := int64(t.width)
	rows := int64(len(t.rows)) + 1 // the header row, empty or not
	// Each row is "|" plus " cell |" per column and a newline; the delimiter row is
	// "|" plus " --- |" per column; the final newline is dropped.
	return rows*(2+3*width) + t.cellBytes + 2 + 6*width - 1
}

// check reports whether the table still fits its limit.
func (t *Table) check() error {
	if t.max >= 0 && t.size() > t.max {
		return &convert.LimitError{Limit: convert.LimitOutput, Max: t.max}
	}
	return nil
}

// Markdown renders the table, padding rows to the widest row. A table without
// rows or header renders as "".
func (t *Table) Markdown() string {
	if !t.hasHeader && len(t.rows) == 0 {
		return ""
	}
	var b strings.Builder
	row := func(cells []string) {
		b.WriteString("|")
		for column := range t.width {
			cell := ""
			if column < len(cells) {
				cell = cells[column]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
	}
	row(t.header)
	b.WriteString("|" + strings.Repeat(" --- |", t.width) + "\n")
	for _, cells := range t.rows {
		row(cells)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
