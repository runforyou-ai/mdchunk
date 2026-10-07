package html

import (
	"errors"
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
// content: a space, the content, a space and a pipe.
const cellOverhead = 3

// nodeBytes is what copying one node is assumed to cost against the expansion limit.
const nodeBytes = 64

// silent are elements whose text the converter drops.
var silent = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Template: true, atom.Noscript: true}

// shownAttributes are attributes whose values appear in the Markdown.
var shownAttributes = map[string]bool{"href": true, "src": true, "alt": true, "title": true}

// inlineCode are the elements the converter renders as inline code.
var inlineCode = map[atom.Atom]bool{atom.Code: true, atom.Var: true, atom.Samp: true, atom.Kbd: true, atom.Tt: true}

// prepareTables expands every table, innermost first, into a rectangular grid
// without spans and escapes pipes inside inline code in cells. Before a table
// is expanded, a lower bound of its rendered size is checked against what is
// left of limits.MaxOutputBytes, and the copies it makes against what is left
// of limits.MaxExpandedBytes; a table that cannot fit returns a
// *convert.LimitError.
func prepareTables(doc *xhtml.Node, limits convert.Limits) error {
	output, expanded := limits.MaxOutputBytes, limits.MaxExpandedBytes
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
		size, copies, err := expandTable(table, output, expanded)
		switch {
		case errors.Is(err, errOutput):
			return &convert.LimitError{Limit: convert.LimitOutput, Max: limits.MaxOutputBytes}
		case errors.Is(err, errExpanded):
			return &convert.LimitError{Limit: convert.LimitExpanded, Max: limits.MaxExpandedBytes}
		}
		if output >= 0 {
			output -= size
		}
		if expanded >= 0 {
			expanded -= copies
		}
	}
	return nil
}

// Budget errors of expandTable.
var (
	errOutput   = errors.New("table output exceeds the budget")
	errExpanded = errors.New("table copies exceed the budget")
)

// row is a table row and the index of the last row in its row group.
type row struct {
	node     *xhtml.Node
	groupEnd int
}

// expandTable rewrites the rows of table so each holds one cell per column,
// repeating spanned cells. It returns a lower bound of the table's rendered
// size and the bytes its copies are assumed to take. When either exceeds its
// budget (negative means unlimited) it returns errOutput or errExpanded and
// leaves the table unchanged.
func expandTable(table *xhtml.Node, output, expanded int64) (int64, int64, error) {
	rows := tableRows(table)
	if len(rows) == 0 {
		return 0, 0, nil
	}
	// Bound the grid and its content before allocating: active widths per row
	// come from a difference array over the rows each cell spans.
	widths := make([]int64, len(rows)+1)
	var content, copies, area int64
	for i, r := range rows {
		for _, cell := range rowCells(r.node) {
			colspan, rowspan := spans(cell, r.groupEnd-i+1)
			widths[i] += int64(colspan)
			widths[i+rowspan] -= int64(colspan)
			text, nodes := measureCell(cell)
			slots := int64(colspan) * int64(rowspan)
			content += text * slots
			copies += nodes * nodeBytes * (slots - 1)
			area += slots
		}
	}
	var width, active int64
	for i := range rows {
		active += widths[i]
		width = max(width, active)
	}
	size := content + int64(len(rows))*width*cellOverhead
	copies += max(int64(len(rows))*width-area, 0) * nodeBytes // padding cells
	if output >= 0 && size > output {
		return 0, 0, errOutput
	}
	if expanded >= 0 && copies > expanded {
		return 0, 0, errExpanded
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
	return size, copies, nil
}

// measureCell returns a lower bound of the Markdown a cell's content renders
// to (its non-whitespace text outside script and style, and the values of
// link and image attributes) and the number of nodes copying it makes.
func measureCell(cell *xhtml.Node) (int64, int64) {
	var text, nodes int64
	var walk func(n *xhtml.Node, quiet bool)
	walk = func(n *xhtml.Node, quiet bool) {
		nodes++
		switch n.Type {
		case xhtml.TextNode:
			if !quiet {
				for _, r := range n.Data {
					if !unicode.IsSpace(r) {
						text++
					}
				}
			}
		case xhtml.ElementNode:
			quiet = quiet || silent[n.DataAtom]
			for _, attr := range n.Attr {
				if shownAttributes[attr.Key] && !quiet {
					text += int64(len(strings.TrimSpace(attr.Val)))
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child, quiet)
		}
	}
	walk(cell, false)
	return text, nodes
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
