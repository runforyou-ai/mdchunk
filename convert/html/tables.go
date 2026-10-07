package html

import (
	"strings"
	"unicode"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/runforyou-ai/mdchunk/convert"
)

// Span limits from the HTML standard; browsers clamp larger values.
const (
	maxColspan = 1000
	maxRowspan = 65534
)

// cellOverhead is the least Markdown a table cell renders to besides its
// content ("|  " and a space).
const cellOverhead = 3

// inlineCode are the elements the converter renders as inline code.
var inlineCode = map[atom.Atom]bool{atom.Code: true, atom.Var: true, atom.Samp: true, atom.Kbd: true, atom.Tt: true}

// prepareTables expands every table, innermost first, into a rectangular grid
// without spans and escapes pipes inside inline code in cells. Before a table
// is expanded, a lower bound of its rendered size is checked against what is
// left of maxOutput; a table that cannot fit returns a *convert.LimitError.
func prepareTables(doc *xhtml.Node, maxOutput int64) error {
	budget := maxOutput
	var tables []*xhtml.Node
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if n.Type == xhtml.ElementNode && n.DataAtom == atom.Table {
			tables = append(tables, n)
		}
	}
	walk(doc)
	for _, table := range tables {
		cost, ok := expandTable(table, budget)
		if !ok {
			return &convert.LimitError{Limit: convert.LimitOutput, Max: maxOutput}
		}
		if budget >= 0 {
			budget -= cost
		}
	}
	return nil
}

// row is a table row and the index of the last row in its row group.
type row struct {
	node     *xhtml.Node
	groupEnd int
}

// expandTable rewrites the rows of table so each holds one cell per column,
// repeating spanned cells. It returns the lower bound of the table's rendered
// size, or false when that exceeds budget (budget < 0 means unlimited); then
// the table is left unchanged.
func expandTable(table *xhtml.Node, budget int64) (int64, bool) {
	rows := tableRows(table)
	if len(rows) == 0 {
		return 0, true
	}
	// Bound the grid and its content before allocating: active widths per row
	// come from a difference array over the rows each cell spans.
	widths := make([]int64, len(rows)+1)
	var content int64
	for i, r := range rows {
		for _, cell := range rowCells(r.node) {
			colspan, rowspan := spans(cell, r.groupEnd-i+1)
			widths[i] += int64(colspan)
			widths[i+rowspan] -= int64(colspan)
			content += cellCost(cell) * int64(colspan) * int64(rowspan)
		}
	}
	var width, active int64
	for i := range rows {
		active += widths[i]
		width = max(width, active)
	}
	cost := content + int64(len(rows))*width*cellOverhead
	if budget >= 0 && cost > budget {
		return 0, false
	}

	grid := make([][]*xhtml.Node, len(rows))
	for i, r := range rows {
		column := 0
		for _, cell := range rowCells(r.node) {
			for column < len(grid[i]) && grid[i][column] != nil {
				column++
			}
			colspan, rowspan := spans(cell, r.groupEnd-i+1)
			for k := i; k < i+rowspan; k++ {
				for len(grid[k]) < column+colspan {
					grid[k] = append(grid[k], nil)
				}
				for c := column; c < column+colspan; c++ {
					grid[k][c] = cell
				}
			}
			column += colspan
		}
	}
	gridWidth := 0
	for i := range grid {
		gridWidth = max(gridWidth, len(grid[i]))
	}
	for i, r := range rows {
		for _, cell := range rowCells(r.node) {
			r.node.RemoveChild(cell)
		}
		for c := range gridWidth {
			var cell *xhtml.Node
			if c < len(grid[i]) && grid[i][c] != nil {
				cell = clone(grid[i][c])
				cell.Attr = withoutSpans(cell.Attr)
			} else {
				cell = &xhtml.Node{Type: xhtml.ElementNode, Data: "td", DataAtom: atom.Td}
			}
			escapeCodePipes(cell)
			r.node.AppendChild(cell)
		}
	}
	return cost, true
}

// cellCost is a lower bound of the Markdown a cell renders to: its
// non-whitespace text, one byte per element and the cell overhead.
func cellCost(cell *xhtml.Node) int64 {
	var cost int64 = cellOverhead
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		switch n.Type {
		case xhtml.TextNode:
			for _, r := range n.Data {
				if !unicode.IsSpace(r) {
					cost++
				}
			}
		case xhtml.ElementNode:
			cost++
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(cell)
	return cost
}

// tableRows returns the rows of table with their row group ends, including
// rows in row groups but not those of nested tables. Consecutive rows directly
// under the table form one group.
func tableRows(table *xhtml.Node) []row {
	var rows []row
	group := func(start int) {
		for i := start; i < len(rows); i++ {
			rows[i].groupEnd = len(rows) - 1
		}
	}
	direct := -1
	for child := table.FirstChild; child != nil; child = child.NextSibling {
		switch child.DataAtom {
		case atom.Tr:
			if direct < 0 {
				direct = len(rows)
			}
			rows = append(rows, row{node: child})
		case atom.Thead, atom.Tbody, atom.Tfoot:
			if direct >= 0 {
				group(direct)
				direct = -1
			}
			start := len(rows)
			for r := child.FirstChild; r != nil; r = r.NextSibling {
				if r.DataAtom == atom.Tr {
					rows = append(rows, row{node: r})
				}
			}
			group(start)
		}
	}
	if direct >= 0 {
		group(direct)
	}
	return rows
}

// rowCells returns the td and th children of a row.
func rowCells(tr *xhtml.Node) []*xhtml.Node {
	var cells []*xhtml.Node
	for child := tr.FirstChild; child != nil; child = child.NextSibling {
		if child.DataAtom == atom.Td || child.DataAtom == atom.Th {
			cells = append(cells, child)
		}
	}
	return cells
}

// spans returns a cell's clamped colspan and rowspan. A rowspan of 0 extends
// to the end of the row group, which is remaining rows long, and no span
// crosses it.
func spans(cell *xhtml.Node, remaining int) (int, int) {
	colspan, rowspan := 1, 1
	for _, attr := range cell.Attr {
		value, ok := spanValue(attr.Val)
		if !ok {
			continue
		}
		switch attr.Key {
		case "colspan":
			colspan = min(max(value, 1), maxColspan)
		case "rowspan":
			rowspan = min(value, maxRowspan)
			if value == 0 {
				rowspan = remaining
			}
		}
	}
	return colspan, min(max(rowspan, 1), remaining)
}

// spanValue parses the leading digits of a span attribute as browsers do,
// saturating long numbers.
func spanValue(value string) (int, bool) {
	value = strings.TrimSpace(value)
	n, digits := 0, 0
	for digits < len(value) && value[digits] >= '0' && value[digits] <= '9' {
		if n < maxRowspan {
			n = n*10 + int(value[digits]-'0')
		}
		digits++
	}
	return n, digits > 0
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

// escapeCodePipes escapes '|' in inline code within a cell, which GFM tables
// require. Nested tables were prepared on their own and are skipped.
func escapeCodePipes(cell *xhtml.Node) {
	var walk func(n *xhtml.Node, inCode bool)
	walk = func(n *xhtml.Node, inCode bool) {
		if n.Type == xhtml.TextNode && inCode {
			n.Data = strings.ReplaceAll(n.Data, "|", `\|`)
		}
		inCode = inCode || inlineCode[n.DataAtom]
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			if child.DataAtom != atom.Table {
				walk(child, inCode)
			}
		}
	}
	walk(cell, false)
}
