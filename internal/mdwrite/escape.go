package mdwrite

import (
	"strings"

	"github.com/runforyou-ai/mdchunk/convert"
)

// Paragraph escapes source text so that none of its lines opens a Markdown
// block structure: indentation is removed and a line-leading marker is
// backslash-escaped.
func Paragraph(text string) string {
	lines := strings.Split(text, "\n")
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

// Cell collapses whitespace in a table cell and escapes pipes.
func Cell(text string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(text), " "), "|", `\|`)
}

// Table collects rows for a GFM table. Rows whose cells are all empty are skipped.
type Table struct {
	rows  [][]string
	width int
	size  int64
	max   int64
}

// NewTable returns a Table whose rendering may not exceed maxBytes; maxBytes < 0 means unlimited.
func NewTable(maxBytes int64) *Table {
	return &Table{max: maxBytes}
}

// Add appends a row, escaping its cells. It returns a *convert.LimitError once
// the table would exceed its limit.
func (t *Table) Add(row []string) error {
	cells := make([]string, len(row))
	blank := true
	for i, cell := range row {
		cells[i] = Cell(cell)
		blank = blank && cells[i] == ""
		t.size += int64(len(cells[i]) + 3)
	}
	if blank {
		return nil
	}
	t.rows = append(t.rows, cells)
	t.width = max(t.width, len(cells))
	if t.max >= 0 && t.size+int64(len(t.rows)*(t.width*6+2)) > t.max {
		return &convert.LimitError{Limit: convert.LimitOutput, Max: t.max}
	}
	return nil
}

// Len returns the number of rows added.
func (t *Table) Len() int {
	return len(t.rows)
}

// Markdown renders the table with the first row as header, or under an empty
// header row when header is false. Rows are padded to the widest row.
// An empty table renders as "".
func (t *Table) Markdown(header bool) string {
	if len(t.rows) == 0 {
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
	rows := t.rows
	if header {
		row(rows[0])
		rows = rows[1:]
	} else {
		row(nil)
	}
	b.WriteString("|" + strings.Repeat(" --- |", t.width) + "\n")
	for _, cells := range rows {
		row(cells)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
