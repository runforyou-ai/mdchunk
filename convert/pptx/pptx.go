// Package pptx converts PowerPoint presentations (.pptx) to Markdown.
//
// Each slide becomes a convert.KindSlide section holding its title
// placeholder as a level-1 heading, its text boxes, tables, chart data as
// tables and its speaker notes. Hidden slides are skipped unless
// Options.IncludeHidden is set; their sections are still recorded, empty, so
// slide numbers match the presentation.
package pptx

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/ooxml"
	"github.com/runforyou-ai/mdchunk/internal/source"
)

// Options configures the PowerPoint converter. The zero value is the default.
type Options struct {
	convert.Limits
	// SkipNotes leaves out speaker notes.
	SkipNotes bool
	// IncludeHidden converts hidden slides too.
	IncludeHidden bool
}

// Converter converts PowerPoint presentations. It is safe for concurrent use.
type Converter struct {
	opts Options
}

// New returns a PowerPoint converter.
func New(opts Options) *Converter {
	return &Converter{opts: opts}
}

// slide renders one slide's parts.
type slide struct {
	ctx           context.Context
	pkg           *ooxml.Package
	relationships map[string]ooxml.Relationship
	limits        convert.Limits
	title         string
}

// Convert converts in to Markdown with one section per slide.
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
	presentation, err := pkg.RequirePart("ppt/presentation.xml")
	if err != nil {
		return convert.Document{}, err
	}
	if presentation.Child("presentation") == nil {
		return convert.Document{}, fmt.Errorf("%w: ppt/presentation.xml has no presentation", convert.ErrCorrupt)
	}
	relationships, err := pkg.Relationships("ppt/presentation.xml")
	if err != nil {
		return convert.Document{}, err
	}
	w := mdwrite.New(limits.MaxOutputBytes)
	var sections []convert.Section
	number := 0
	for _, item := range presentation.Child("presentation").Child("sldIdLst").Elements() {
		if item.Name != "sldId" {
			continue
		}
		number++
		blocks, title, err := c.slide(ctx, pkg, relationships[item.Attr("r:id")].Target, limits)
		if err != nil {
			return convert.Document{}, err
		}
		// Blocks of consecutive slides are separated by a blank line outside both sections.
		if len(blocks) > 0 && w.Len() > 0 {
			w.WriteString("\n\n")
		}
		start := w.Len()
		w.WriteString(strings.Join(blocks, "\n\n"))
		sections = append(sections, convert.Section{Kind: convert.KindSlide, Number: number, Name: title, Start: start, End: w.Len()})
	}
	if err := w.Err(); err != nil {
		return convert.Document{}, err
	}
	if err := ctx.Err(); err != nil {
		return convert.Document{}, err
	}
	return convert.Document{Markdown: w.String(), Sections: sections}, nil
}

// slide renders the slide at target and returns its blocks and title.
func (c *Converter) slide(ctx context.Context, pkg *ooxml.Package, target string, limits convert.Limits) ([]string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	root, err := pkg.RequirePart(target)
	if err != nil {
		return nil, "", err
	}
	if root.Child("sld") == nil {
		return nil, "", fmt.Errorf("%w: %s is not a slide", convert.ErrCorrupt, target)
	}
	if show := root.Child("sld").Attr("show"); (show == "0" || show == "false") && !c.opts.IncludeHidden {
		return nil, "", nil
	}
	relationships, err := pkg.Relationships(target)
	if err != nil {
		return nil, "", err
	}
	s := &slide{ctx: ctx, pkg: pkg, relationships: relationships, limits: limits}
	var blocks []string
	if err := s.blocks(root.Child("sld").Child("cSld").Child("spTree"), &blocks); err != nil {
		return nil, "", err
	}
	if c.opts.SkipNotes {
		return blocks, s.title, nil
	}
	// Only the notes body placeholder is speaker notes; the slide image and number are not.
	ids := make([]string, 0, len(relationships))
	for id := range relationships {
		ids = append(ids, id)
	}
	// Relationship ids such as rId2 and rId10 sort by length first, then text.
	slices.SortFunc(ids, func(a, b string) int { return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b)) })
	for _, id := range ids {
		item := relationships[id]
		if item.Type != "notesSlide" {
			continue
		}
		notes, err := pkg.RequirePart(item.Target)
		if err != nil {
			return nil, "", err
		}
		for _, shape := range notes.Child("notes").Child("cSld").Child("spTree").Elements() {
			if shape.Name == "sp" && shape.Child("nvSpPr").Child("nvPr").Child("ph").Attr("type") == "body" {
				if text := shapeText(shape); text != "" {
					blocks = append(blocks, mdwrite.Paragraph(text))
				}
			}
		}
	}
	return blocks, s.title, nil
}

// blocks renders text boxes, tables and charts in shape order, unwrapping groups.
func (s *slide) blocks(tree *ooxml.Node, blocks *[]string) error {
	for _, shape := range tree.Elements() {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		switch shape.Name {
		case "sp":
			text := shapeText(shape)
			if text == "" {
				continue
			}
			if kind := shape.Child("nvSpPr").Child("nvPr").Child("ph").Attr("type"); kind == "title" || kind == "ctrTitle" {
				title := strings.Join(strings.Fields(text), " ")
				if s.title == "" {
					s.title = title
				}
				*blocks = append(*blocks, "# "+title)
				continue
			}
			*blocks = append(*blocks, mdwrite.Paragraph(text))
		case "graphicFrame":
			data := shape.Child("graphic").Child("graphicData")
			if chart := data.Child("chart"); chart != nil {
				text, err := s.chart(s.relationships[chart.Attr("r:id")].Target)
				if err != nil {
					return err
				}
				if text != "" {
					*blocks = append(*blocks, text)
				}
				continue
			}
			table := mdwrite.NewTable(s.limits.MaxOutputBytes, true)
			for _, row := range data.Child("tbl").Elements() {
				if row.Name != "tr" {
					continue
				}
				var cells []string
				for _, cell := range row.Elements() {
					if cell.Name == "tc" {
						cells = append(cells, slideText(cell.Child("txBody")))
					}
				}
				if err := table.Add(cells); err != nil {
					return err
				}
			}
			if markdown := table.Markdown(); markdown != "" {
				*blocks = append(*blocks, markdown)
			}
		case "grpSp":
			if err := s.blocks(shape, blocks); err != nil {
				return err
			}
		case "AlternateContent":
			if err := s.blocks(shape.Child("Choice"), blocks); err != nil {
				return err
			}
		}
	}
	return nil
}

// chart renders a chart's title and data. Category series form one table with
// categories in the first column and one column per series; scatter and
// bubble series form a table with one row per point: series, x, y and size.
func (s *slide) chart(target string) (string, error) {
	root, err := s.pkg.RequirePart(target)
	if err != nil {
		return "", err
	}
	chart := root.Child("chartSpace").Child("chart")
	var categorySeries, pointSeries []*ooxml.Node
	for _, plot := range chart.Child("plotArea").Elements() {
		for _, series := range plot.Elements() {
			switch {
			case series.Name != "ser":
			case series.Child("val") != nil:
				categorySeries = append(categorySeries, series)
			case series.Child("yVal") != nil:
				pointSeries = append(pointSeries, series)
			}
		}
	}
	var blocks []string
	if title := chartTitle(chart.Child("title")); title != "" {
		blocks = append(blocks, mdwrite.Paragraph(title))
	}
	if len(categorySeries) > 0 {
		table, err := s.categoryTable(categorySeries)
		if err != nil {
			return "", err
		}
		blocks = append(blocks, table)
	}
	if len(pointSeries) > 0 {
		table, err := s.pointTable(pointSeries)
		if err != nil {
			return "", err
		}
		blocks = append(blocks, table)
	}
	if len(categorySeries)+len(pointSeries) == 0 {
		return "", nil
	}
	return strings.Join(blocks, "\n\n"), nil
}

// chartTitle returns a chart title from rich text or a cached string reference.
func chartTitle(title *ooxml.Node) string {
	if text := strings.Join(strings.Fields(slideText(title)), " "); text != "" {
		return text
	}
	return strings.Join(cacheValues(title.Child("tx")), " ")
}

// seriesName returns a series name from its reference cache or literal value.
func seriesName(series *ooxml.Node) string {
	if names := cacheValues(series.Child("tx")); len(names) > 0 {
		return names[0]
	}
	if value := series.Child("tx").Child("v"); value != nil {
		return value.Text
	}
	return ""
}

// categoryTable renders category series: categories in the first column, one column per series.
func (s *slide) categoryTable(series []*ooxml.Node) (string, error) {
	header := []string{""}
	var columns [][]string
	var labels []string
	for _, item := range series {
		header = append(header, seriesName(item))
		columns = append(columns, cacheValues(item.Child("val")))
		if len(labels) == 0 {
			labels = cacheValues(item.Child("cat"))
		}
	}
	table := mdwrite.NewTable(s.limits.MaxOutputBytes, true)
	if err := table.SetHeader(header); err != nil {
		return "", err
	}
	count := len(labels)
	for _, column := range columns {
		count = max(count, len(column))
	}
	for index := range count {
		if err := s.ctx.Err(); err != nil {
			return "", err
		}
		row := []string{at(labels, index)}
		for _, column := range columns {
			row = append(row, at(column, index))
		}
		if err := table.Add(row); err != nil {
			return "", err
		}
	}
	return table.Markdown(), nil
}

// pointTable renders scatter and bubble series with one row per point.
func (s *slide) pointTable(series []*ooxml.Node) (string, error) {
	header := []string{"", "x", "y"}
	bubbles := slices.ContainsFunc(series, func(item *ooxml.Node) bool { return item.Child("bubbleSize") != nil })
	if bubbles {
		header = append(header, "size")
	}
	table := mdwrite.NewTable(s.limits.MaxOutputBytes, true)
	if err := table.SetHeader(header); err != nil {
		return "", err
	}
	for _, item := range series {
		name := seriesName(item)
		xs, ys, sizes := cacheValues(item.Child("xVal")), cacheValues(item.Child("yVal")), cacheValues(item.Child("bubbleSize"))
		for index := range max(len(xs), len(ys), len(sizes)) {
			if err := s.ctx.Err(); err != nil {
				return "", err
			}
			row := []string{name, at(xs, index), at(ys, index)}
			if bubbles {
				row = append(row, at(sizes, index))
			}
			if err := table.Add(row); err != nil {
				return "", err
			}
		}
	}
	return table.Markdown(), nil
}

// at returns values[index], or "" beyond its end.
func at(values []string, index int) string {
	if index < len(values) {
		return values[index]
	}
	return ""
}

// maxPoints bounds the data point index read from a chart cache.
const maxPoints = 1 << 16

// cacheValues returns a chart cache's values by point index; missing points are empty.
func cacheValues(node *ooxml.Node) []string {
	var values []string
	for _, child := range node.Elements() {
		if child.Name != "pt" {
			values = append(values, cacheValues(child)...)
			continue
		}
		index, err := strconv.Atoi(child.Attr("idx"))
		if err != nil || index < 0 || index >= maxPoints {
			continue
		}
		for len(values) <= index {
			values = append(values, "")
		}
		if value := child.Child("v"); value != nil {
			values[index] = value.Text
		}
	}
	return values
}

// shapeText returns the non-empty paragraphs of a shape, one per line.
func shapeText(shape *ooxml.Node) string {
	var paragraphs []string
	for _, paragraph := range shape.Child("txBody").Elements() {
		if text := strings.TrimSpace(slideText(paragraph)); paragraph.Name == "p" && text != "" {
			paragraphs = append(paragraphs, text)
		}
	}
	return strings.Join(paragraphs, "\n")
}

// slideText joins text runs and fields; line breaks become newlines and paragraphs are separated by spaces.
func slideText(node *ooxml.Node) string {
	var builder strings.Builder
	for _, child := range node.Elements() {
		switch child.Name {
		case "t":
			builder.WriteString(child.Text)
		case "br":
			builder.WriteString("\n")
		case "p":
			builder.WriteString(slideText(child) + " ")
		case "pPr", "rPr", "endParaRPr":
		default:
			builder.WriteString(slideText(child))
		}
	}
	return builder.String()
}
