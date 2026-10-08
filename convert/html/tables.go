package html

import (
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

// Copies made by table expansion are charged in memory terms: nodeBytes per
// copied node and textWeight per byte of copied text or attribute, roughly
// what converting the copy costs.
const (
	nodeBytes  = 64
	textWeight = 16
)

// inlineCode are the elements the converter renders as inline code.
var inlineCode = map[atom.Atom]bool{atom.Code: true, atom.Var: true, atom.Samp: true, atom.Kbd: true, atom.Tt: true}

// prepareTables expands every table, innermost first, into a rectangular grid
// without spans and escapes pipes inside inline code in cells. Before a table
// is expanded, the copies it would make are charged against what is left of
// maxExpanded (negative means unlimited); a table that does not fit returns
// an expanded *convert.LimitError. Link and image addresses are charged with
// baseURL, which they resolve against.
func prepareTables(doc *xhtml.Node, maxExpanded int64, baseURL string) error {
	budget := maxExpanded
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
		cost, ok := expandTable(table, budget, int64(len(baseURL)))
		if !ok {
			return &convert.LimitError{Limit: convert.LimitExpanded, Max: maxExpanded}
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
// repeating spanned cells. It returns the cost of the copies, or false when
// that exceeds budget (negative means unlimited); then the table is unchanged.
func expandTable(table *xhtml.Node, budget, baseURL int64) (int64, bool) {
	rows := tableRows(table)
	if len(rows) == 0 {
		return 0, true
	}
	// Charge copies before allocating: active widths per row come from a
	// difference array over the rows each cell spans, and padding cells are
	// what the widest row leaves.
	widths := make([]int64, len(rows)+1)
	var cost, area int64
	for i, r := range rows {
		for _, cell := range rowCells(r.node) {
			colspan, rowspan := spans(cell, r.groupEnd-i+1)
			widths[i] += int64(colspan)
			widths[i+rowspan] -= int64(colspan)
			slots := int64(colspan) * int64(rowspan)
			cost += copyCost(cell, baseURL) * (slots - 1)
			area += slots
		}
	}
	var width, active int64
	for i := range rows {
		active += widths[i]
		width = max(width, active)
	}
	cost += max(int64(len(rows))*width-area, 0) * nodeBytes
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
	for _, r := range rows {
		for _, cell := range rowCells(r.node) {
			r.node.RemoveChild(cell)
		}
	}
	// Each cell is prepared once; its first slot reuses it and further slots hold copies.
	placed := map[*xhtml.Node]bool{}
	for i, r := range rows {
		for c := range gridWidth {
			var cell *xhtml.Node
			switch {
			case c >= len(grid[i]) || grid[i][c] == nil:
				cell = &xhtml.Node{Type: xhtml.ElementNode, Data: "td", DataAtom: atom.Td}
			case placed[grid[i][c]]:
				cell = clone(grid[i][c])
			default:
				cell = grid[i][c]
				cell.Attr = withoutSpans(cell.Attr)
				escapeCodePipes(cell)
				placed[cell] = true
			}
			r.node.AppendChild(cell)
		}
	}
	return cost, true
}

// copyCost returns what one copy of cell is charged: its nodes, text and
// attributes, with link and image addresses charged the base URL too.
func copyCost(cell *xhtml.Node, baseURL int64) int64 {
	var cost int64
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		cost += nodeBytes + int64(len(n.Data))*textWeight
		for _, attr := range n.Attr {
			cost += int64(len(attr.Key)+len(attr.Val)) * textWeight
			if attr.Key == "href" || attr.Key == "src" {
				cost += baseURL * textWeight
			}
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
	value = strings.TrimPrefix(strings.TrimSpace(value), "+")
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
