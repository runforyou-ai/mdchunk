package html

import (
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/runforyou-ai/mdchunk/convert"
)

// Span limits from the HTML standard; browsers clamp larger values.
const (
	maxColspan = 1000
	maxRowspan = 65534
)

// minCellBytes is the least Markdown a table cell renders to ("| x "), used to
// turn the output limit into a cell budget before tables are expanded.
const minCellBytes = 3

// prepareTables expands every table into a rectangular grid without spans and
// escapes pipes inside code in cells. It returns a *convert.LimitError when the
// expanded cells could not fit in maxOutput.
func prepareTables(doc *xhtml.Node, maxOutput int64) error {
	budget := int64(-1)
	if maxOutput >= 0 {
		budget = maxOutput / minCellBytes
	}
	var tables []*xhtml.Node
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.DataAtom == atom.Table {
			tables = append(tables, n)
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	for _, table := range tables {
		used, err := expandTable(table, budget)
		if err != nil {
			return &convert.LimitError{Limit: convert.LimitOutput, Max: maxOutput}
		}
		if budget >= 0 {
			budget -= used
		}
	}
	return nil
}

// errBudget reports that a table needs more cells than the budget allows.
type errBudget struct{}

func (errBudget) Error() string { return "table exceeds the cell budget" }

// expandTable rewrites the rows of table so each holds one cell per column,
// repeating spanned cells. It returns the number of cells in the grid.
func expandTable(table *xhtml.Node, budget int64) (int64, error) {
	rows := tableRows(table)
	if len(rows) == 0 {
		return 0, nil
	}
	// Size the grid from the clamped spans before allocating it.
	var cells int64
	for i, row := range rows {
		for _, cell := range rowCells(row) {
			colspan, rowspan := spans(cell, len(rows)-i)
			cells += int64(colspan) * int64(rowspan)
		}
	}
	if budget >= 0 && cells > budget {
		return 0, errBudget{}
	}
	grid := make([][]*xhtml.Node, len(rows))
	width := 0
	for i, row := range rows {
		column := 0
		for _, cell := range rowCells(row) {
			for column < len(grid[i]) && grid[i][column] != nil {
				column++
			}
			colspan, rowspan := spans(cell, len(rows)-i)
			for r := i; r < i+rowspan; r++ {
				for len(grid[r]) < column+colspan {
					grid[r] = append(grid[r], nil)
				}
				for c := column; c < column+colspan; c++ {
					grid[r][c] = cell
				}
			}
			column += colspan
		}
		width = max(width, len(grid[i]))
	}
	for i := range grid {
		width = max(width, len(grid[i]))
	}
	if total := int64(len(rows)) * int64(width); budget >= 0 && total > budget {
		return 0, errBudget{}
	}
	for i, row := range rows {
		for _, cell := range rowCells(row) {
			row.RemoveChild(cell)
		}
		for c := 0; c < width; c++ {
			var cell *xhtml.Node
			if c < len(grid[i]) && grid[i][c] != nil {
				cell = clone(grid[i][c])
				cell.Attr = withoutSpans(cell.Attr)
			} else {
				cell = &xhtml.Node{Type: xhtml.ElementNode, Data: "td", DataAtom: atom.Td}
			}
			escapeCodePipes(cell)
			row.AppendChild(cell)
		}
	}
	return int64(len(rows)) * int64(width), nil
}

// tableRows returns the rows of table, including those in its row groups but
// not those of nested tables.
func tableRows(table *xhtml.Node) []*xhtml.Node {
	var rows []*xhtml.Node
	for child := table.FirstChild; child != nil; child = child.NextSibling {
		switch child.DataAtom {
		case atom.Tr:
			rows = append(rows, child)
		case atom.Thead, atom.Tbody, atom.Tfoot:
			for row := child.FirstChild; row != nil; row = row.NextSibling {
				if row.DataAtom == atom.Tr {
					rows = append(rows, row)
				}
			}
		}
	}
	return rows
}

// rowCells returns the td and th children of row.
func rowCells(row *xhtml.Node) []*xhtml.Node {
	var cells []*xhtml.Node
	for child := row.FirstChild; child != nil; child = child.NextSibling {
		if child.DataAtom == atom.Td || child.DataAtom == atom.Th {
			cells = append(cells, child)
		}
	}
	return cells
}

// spans returns a cell's clamped colspan and rowspan; rowspan 0 or beyond the
// table extends to the last row, which is remaining rows away.
func spans(cell *xhtml.Node, remaining int) (int, int) {
	colspan, rowspan := 1, 1
	for _, attr := range cell.Attr {
		value, err := strconv.Atoi(strings.TrimSpace(attr.Val))
		if err != nil {
			continue
		}
		switch attr.Key {
		case "colspan":
			colspan = min(max(value, 1), maxColspan)
		case "rowspan":
			rowspan = min(value, maxRowspan)
			if value <= 0 {
				rowspan = remaining
			}
		}
	}
	return colspan, min(max(rowspan, 1), remaining)
}

// withoutSpans drops colspan and rowspan attributes.
func withoutSpans(attrs []xhtml.Attribute) []xhtml.Attribute {
	kept := attrs[:0:0]
	for _, attr := range attrs {
		if attr.Key != "colspan" && attr.Key != "rowspan" {
			kept = append(kept, attr)
		}
	}
	return kept
}

// clone deep-copies n without its parent and siblings.
func clone(n *xhtml.Node) *xhtml.Node {
	copied := &xhtml.Node{Type: n.Type, Data: n.Data, DataAtom: n.DataAtom, Namespace: n.Namespace,
		Attr: append([]xhtml.Attribute(nil), n.Attr...)}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		copied.AppendChild(clone(child))
	}
	return copied
}

// escapeCodePipes escapes '|' in code within a cell, which GFM tables require.
func escapeCodePipes(cell *xhtml.Node) {
	var walk func(n *xhtml.Node, inCode bool)
	walk = func(n *xhtml.Node, inCode bool) {
		if n.Type == xhtml.TextNode && inCode {
			n.Data = strings.ReplaceAll(n.Data, "|", `\|`)
		}
		inCode = inCode || n.DataAtom == atom.Code
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child, inCode)
		}
	}
	walk(cell, false)
}
