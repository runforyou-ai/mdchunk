// Package docx converts Word documents (.docx) to Markdown.
//
// Headings come from heading styles, the Title style and outline levels;
// numbered and bulleted paragraphs become list items; tables become GFM
// tables with horizontal spans repeated and vertical merges carrying the
// value down; external hyperlinks become Markdown links. Word documents have
// no fixed pages, so the Document has no sections.
package docx

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/ooxml"
	"github.com/runforyou-ai/mdchunk/internal/source"
)

// maxListDepth is the deepest list level Word supports, counted from 0.
const maxListDepth = 8

// linkTarget encodes the characters an angle-bracket link destination cannot hold.
var linkTarget = strings.NewReplacer("<", "%3C", ">", "%3E", "\n", "%0A")

// styleInfo is what a paragraph style contributes: its parent, heading level and numbering.
type styleInfo struct {
	basedOn     string
	heading     int
	numID, ilvl string
}

// noHeading marks a style that explicitly is not a heading, stopping inheritance.
const noHeading = -1

// maxStyleChain bounds how many basedOn links are followed.
const maxStyleChain = 16

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
	ctx     context.Context
	limits  convert.Limits
	ordered map[string]map[string]bool
	links   map[string]string
	styles  map[string]styleInfo
	// numAbstract maps a numbering instance to its abstract definition, and
	// styleLevels maps an abstract definition's styles to the levels they use.
	numAbstract map[string]string
	styleLevels map[string]map[string]string
	inCell      bool // table cells are not escaped: block markers have no effect there
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
	root, err := pkg.RequirePart("word/document.xml")
	if err != nil {
		return convert.Document{}, err
	}
	body := root.Child("document").Child("body")
	if body == nil {
		return convert.Document{}, fmt.Errorf("%w: word/document.xml has no body", convert.ErrCorrupt)
	}
	d := &document{ctx: ctx, limits: limits, ordered: map[string]map[string]bool{},
		links: map[string]string{}, styles: map[string]styleInfo{}, numAbstract: map[string]string{},
		styleLevels: map[string]map[string]string{}}
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
	if err := ctx.Err(); err != nil {
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
		id := style.Attr("styleId")
		if style.Name != "style" || id == "" {
			continue
		}
		info := styleInfo{basedOn: style.Child("basedOn").Attr("val")}
		name := strings.ToLower(style.Child("name").Attr("val"))
		if name == "title" {
			info.heading = 1
		} else if number, found := strings.CutPrefix(name, "heading "); found {
			info.heading, _ = strconv.Atoi(number)
		}
		// An explicit outline level overrides the name; level 9 is body text.
		if outline, err := strconv.Atoi(style.Child("pPr").Child("outlineLvl").Attr("val")); err == nil {
			info.heading = noHeading
			if outline < 9 {
				info.heading = outline + 1
			}
		}
		list := style.Child("pPr").Child("numPr")
		info.numID, info.ilvl = list.Child("numId").Attr("val"), list.Child("ilvl").Attr("val")
		d.styles[id] = info
	}
	numbering, err := pkg.ReadPart("word/numbering.xml")
	if err != nil {
		return err
	}
	abstract := map[string]map[string]bool{}
	for _, item := range numbering.Child("numbering").Elements() {
		switch item.Name {
		case "abstractNum":
			id := item.Attr("abstractNumId")
			levels, linked := map[string]bool{}, map[string]string{}
			for _, level := range item.Children {
				if level.Name == "lvl" {
					levels[level.Attr("ilvl")] = orderedFormat(level.Child("numFmt").Attr("val"))
					if style := level.Child("pStyle").Attr("val"); style != "" {
						linked[style] = level.Attr("ilvl")
					}
				}
			}
			abstract[id], d.styleLevels[id] = levels, linked
		case "num":
			id, base := item.Attr("numId"), item.Child("abstractNumId").Attr("val")
			d.numAbstract[id] = base
			// Level overrides replace the abstract definition's formats for this instance only.
			levels := map[string]bool{}
			for ilvl, ordered := range abstract[base] {
				levels[ilvl] = ordered
			}
			for _, override := range item.Children {
				if override.Name == "lvlOverride" {
					if format := override.Child("lvl").Child("numFmt"); format != nil {
						levels[override.Attr("ilvl")] = orderedFormat(format.Attr("val"))
					}
				}
			}
			d.ordered[id] = levels
		}
	}
	return nil
}

// orderedFormat reports whether a numbering format counts rather than bullets.
func orderedFormat(format string) bool {
	return format != "bullet" && format != "none" && format != ""
}

// styleHeading returns the heading level of a style or the styles it is based on.
func (d *document) styleHeading(id string) int {
	for range maxStyleChain {
		info, ok := d.styles[id]
		if !ok {
			return 0
		}
		if info.heading == noHeading {
			return 0
		}
		if info.heading > 0 {
			return info.heading
		}
		id = info.basedOn
	}
	return 0
}

// styleNumbering returns the numbering instance and level a style applies.
// Along the basedOn chain the nearest numId and the nearest ilvl apply
// separately; a level linked to the style by the abstract numbering
// definition replaces the style's ilvl.
func (d *document) styleNumbering(id string) (string, string) {
	numID, ilvl, definer := "", "", ""
	current := id
	for range maxStyleChain {
		info, ok := d.styles[current]
		if !ok {
			break
		}
		if numID == "" && info.numID != "" {
			numID, definer = info.numID, current
		}
		if ilvl == "" {
			ilvl = info.ilvl
		}
		current = info.basedOn
	}
	if numID == "" {
		return "", ""
	}
	linked := d.styleLevels[d.numAbstract[numID]]
	if level := cmp.Or(linked[id], linked[definer]); level != "" {
		ilvl = level
	}
	return numID, ilvl
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
	style := properties.Child("pStyle").Attr("val")
	level := d.styleHeading(style)
	if outline, err := strconv.Atoi(properties.Child("outlineLvl").Attr("val")); err == nil {
		level = 0
		if outline < 9 {
			level = outline + 1
		}
	}
	if level > 0 {
		return strings.Repeat("#", min(level, 6)) + " " + strings.Join(strings.Fields(text), " ")
	}
	if d.inCell {
		return text
	}
	// Direct numbering overrides the style's; numId 0 removes numbering and a missing level is 0.
	numID, ilvl := d.styleNumbering(style)
	if list := properties.Child("numPr"); list != nil {
		if id := list.Child("numId").Attr("val"); id != "" {
			numID = id
		}
		if value := list.Child("ilvl").Attr("val"); value != "" {
			ilvl = value
		}
	}
	if numID != "" && numID != "0" {
		if ilvl == "" {
			ilvl = "0"
		}
		depth, _ := strconv.Atoi(ilvl)
		marker := "- "
		if d.ordered[numID][ilvl] {
			marker = "1. "
		}
		// Three spaces per level reach the content of both "1. " and "- " parents.
		indent := strings.Repeat("   ", min(max(depth, 0), maxListDepth))
		return indent + marker + strings.ReplaceAll(mdwrite.Paragraph(text), "\n", "\n"+indent+"   ")
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
			builder.WriteString("[" + text + "](<" + linkTarget.Replace(target) + ">)")
		case "pPr", "rPr", "drawing", "pict", "object", "Fallback":
		default:
			d.text(child, builder)
		}
	}
}

// table renders a table; horizontal spans repeat the cell and vertical merges carry the value down.
func (d *document) table(node *ooxml.Node) (string, error) {
	table := mdwrite.NewTable(d.limits.MaxOutputBytes, true)
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
	return table.Markdown(), nil
}
