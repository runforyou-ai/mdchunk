// Package docx converts Word documents (.docx) to Markdown.
//
// Headings come from heading styles, the Title style and outline levels;
// numbered and bulleted paragraphs become list items; tables become GFM
// tables with horizontal spans repeated and vertical merges carrying the
// value down; external hyperlinks become Markdown links. Word documents have
// no fixed pages, so the Document has no sections.
package docx

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/ooxml"
	"github.com/runforyou-ai/mdchunk/internal/source"
)

// Options configures the Word converter. The zero value is the default.
type Options struct {
	convert.Limits
}

// Converter converts Word documents. It is safe for concurrent use.
type Converter struct {
	opts Options
}

// New returns a Word converter.
func New(opts Options) *Converter {
	return &Converter{opts: opts}
}

// document holds what rendering a body needs: heading styles, list formats and link targets.
type document struct {
	ctx      context.Context
	limits   convert.Limits
	headings map[string]int
	ordered  map[string]map[string]bool
	links    map[string]string
	inCell   bool // table cells are not escaped: block markers have no effect there
}

// Convert converts in to Markdown.
func (c *Converter) Convert(ctx context.Context, in convert.Input) (convert.Document, error) {
	limits := c.opts.Effective()
	data, err := source.Read(ctx, in, limits.MaxBytes)
	if err != nil {
		return convert.Document{}, err
	}
	if len(data) == 0 {
		return convert.Document{}, fmt.Errorf("%w: empty file", convert.ErrCorrupt)
	}
	pkg, err := ooxml.Open(data, limits.MaxExpandedBytes)
	if err != nil {
		return convert.Document{}, err
	}
	root, err := pkg.ReadPart("word/document.xml")
	if err != nil {
		return convert.Document{}, err
	}
	body := root.Child("document").Child("body")
	if body == nil {
		return convert.Document{}, fmt.Errorf("%w: word/document.xml has no body", convert.ErrCorrupt)
	}
	d := &document{ctx: ctx, limits: limits, headings: map[string]int{}, ordered: map[string]map[string]bool{}, links: map[string]string{}}
	if err := d.load(pkg); err != nil {
		return convert.Document{}, err
	}
	w := mdwrite.New(limits.MaxOutputBytes)
	var blocks []string
	if err := d.blocks(body, &blocks); err != nil {
		return convert.Document{}, err
	}
	w.WriteString(strings.Join(blocks, "\n\n"))
	if err := w.Err(); err != nil {
		return convert.Document{}, err
	}
	return convert.Document{Markdown: w.String()}, nil
}

// load reads hyperlink targets, heading styles and list formats.
func (d *document) load(pkg *ooxml.Package) error {
	relationships, err := pkg.Relationships("word/document.xml")
	if err != nil {
		return err
	}
	for id, item := range relationships {
		if item.External {
			d.links[id] = item.Target
		}
	}
	styles, err := pkg.ReadPart("word/styles.xml")
	if err != nil {
		return err
	}
	for _, style := range styles.Child("styles").Elements() {
		name := strings.ToLower(style.Child("name").Attr("val"))
		level := 0
		if name == "title" {
			level = 1
		} else if number, found := strings.CutPrefix(name, "heading "); found {
			level, _ = strconv.Atoi(number)
		} else if outline, err := strconv.Atoi(style.Child("pPr").Child("outlineLvl").Attr("val")); err == nil && outline < 9 {
			level = outline + 1
		}
		if style.Name == "style" && level > 0 {
			d.headings[style.Attr("styleId")] = level
		}
	}
	numbering, err := pkg.ReadPart("word/numbering.xml")
	if err != nil {
		return err
	}
	abstract := map[string]map[string]bool{}
	for _, item := range numbering.Child("numbering").Elements() {
		switch item.Name {
		case "abstractNum":
			levels := map[string]bool{}
			for _, level := range item.Children {
				if level.Name == "lvl" {
					format := level.Child("numFmt").Attr("val")
					levels[level.Attr("ilvl")] = format != "bullet" && format != "none" && format != ""
				}
			}
			abstract[item.Attr("abstractNumId")] = levels
		case "num":
			d.ordered[item.Attr("numId")] = abstract[item.Child("abstractNumId").Attr("val")]
		}
	}
	return nil
}

// blocks renders paragraphs and tables in document order, unwrapping content controls.
func (d *document) blocks(node *ooxml.Node, blocks *[]string) error {
	for _, child := range node.Children {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		switch child.Name {
		case "p":
			if block := d.paragraph(child); block != "" {
				*blocks = append(*blocks, block)
			}
		case "tbl":
			block, err := d.table(child)
			if err != nil {
				return err
			}
			if block != "" {
				*blocks = append(*blocks, block)
			}
		case "sectPr":
		default:
			if err := d.blocks(child, blocks); err != nil {
				return err
			}
		}
	}
	return nil
}

// paragraph renders a paragraph by its style, outline level and numbering.
func (d *document) paragraph(node *ooxml.Node) string {
	var builder strings.Builder
	d.text(node, &builder)
	text := strings.TrimSpace(builder.String())
	if text == "" {
		return ""
	}
	properties := node.Child("pPr")
	level := d.headings[properties.Child("pStyle").Attr("val")]
	if outline, err := strconv.Atoi(properties.Child("outlineLvl").Attr("val")); err == nil && outline < 9 {
		level = outline + 1
	}
	if level > 0 {
		return strings.Repeat("#", min(level, 6)) + " " + strings.Join(strings.Fields(text), " ")
	}
	if d.inCell {
		return text
	}
	if list := properties.Child("numPr"); list != nil {
		depth, _ := strconv.Atoi(list.Child("ilvl").Attr("val"))
		marker := "- "
		if d.ordered[list.Child("numId").Attr("val")][list.Child("ilvl").Attr("val")] {
			marker = "1. "
		}
		indent := strings.Repeat("  ", max(depth, 0))
		return indent + marker + strings.ReplaceAll(mdwrite.Paragraph(text), "\n", "\n"+indent+"  ")
	}
	return mdwrite.Paragraph(text)
}

// text appends runs, tabs and breaks; external hyperlinks become Markdown links.
func (d *document) text(node *ooxml.Node, builder *strings.Builder) {
	for _, child := range node.Children {
		switch child.Name {
		case "t":
			builder.WriteString(child.Text)
		case "tab":
			builder.WriteString(" ")
		case "br", "cr":
			builder.WriteString("\n")
		case "hyperlink":
			target := d.links[child.Attr("r:id")]
			if target == "" {
				d.text(child, builder)
				continue
			}
			var label strings.Builder
			d.text(child, &label)
			text := strings.NewReplacer("[", `\[`, "]", `\]`).Replace(strings.Join(strings.Fields(label.String()), " "))
			builder.WriteString("[" + text + "](<" + strings.ReplaceAll(target, ">", "%3E") + ">)")
		case "pPr", "rPr", "drawing", "pict", "object", "Fallback":
		default:
			d.text(child, builder)
		}
	}
}

// table renders a table; horizontal spans repeat the cell and vertical merges carry the value down.
func (d *document) table(node *ooxml.Node) (string, error) {
	table := mdwrite.NewTable(d.limits.MaxOutputBytes)
	var previous []string
	for _, row := range node.Children {
		if row.Name != "tr" {
			continue
		}
		var cells []string
		for _, cell := range row.Children {
			if cell.Name != "tc" {
				continue
			}
			var paragraphs []string
			outer := d.inCell
			d.inCell = true
			err := d.blocks(cell, &paragraphs)
			d.inCell = outer
			if err != nil {
				return "", err
			}
			text := strings.Join(paragraphs, " ")
			properties := cell.Child("tcPr")
			if merge := properties.Child("vMerge"); merge != nil && merge.Attr("val") != "restart" && len(cells) < len(previous) {
				text = previous[len(cells)]
			}
			span, err := strconv.Atoi(properties.Child("gridSpan").Attr("val"))
			if err != nil || span < 1 {
				span = 1
			}
			for range min(span, 64) {
				cells = append(cells, text)
			}
		}
		if err := table.Add(cells); err != nil {
			return "", err
		}
		previous = cells
	}
	return table.Markdown(true), nil
}
