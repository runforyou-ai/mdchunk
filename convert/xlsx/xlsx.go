// Package xlsx converts Excel workbooks (.xlsx) to Markdown.
//
// Each sheet becomes a convert.KindSheet section holding the sheet name as a
// level-2 heading and a GFM table whose first non-empty row is the header.
// Cells show their displayed, number-formatted values unless
// Options.RawValues is set. Hidden sheets are skipped unless
// Options.IncludeHidden is set; skipped and empty sheets keep an empty
// section so sheet numbers match the workbook.
package xlsx

import (
	"bytes"
	"context"
	"fmt"

	"github.com/xuri/excelize/v2"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/internal/mdwrite"
	"github.com/runforyou-ai/mdchunk/internal/ooxml"
	"github.com/runforyou-ai/mdchunk/internal/source"
)

// Options configures the Excel converter. The zero value is the default.
type Options struct {
	convert.Limits
	// RawValues shows stored cell values instead of number-formatted ones.
	RawValues bool
	// IncludeHidden converts hidden sheets too.
	IncludeHidden bool
}

// Converter converts Excel workbooks. It is safe for concurrent use.
type Converter struct {
	opts Options
}

// New returns an Excel converter.
func New(opts Options) *Converter {
	return &Converter{opts: opts}
}

// Convert converts in to Markdown with one section per sheet.
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
	// The zip reader verifies that entries expand to their declared sizes.
	if limits.MaxExpandedBytes >= 0 {
		var expanded uint64
		for _, entry := range pkg.Files() {
			expanded += entry.UncompressedSize64
			if expanded > uint64(limits.MaxExpandedBytes) {
				return convert.Document{}, &convert.LimitError{Limit: convert.LimitExpanded, Max: limits.MaxExpandedBytes}
			}
		}
	}
	file, err := excelize.OpenReader(bytes.NewReader(data))
	if err != nil {
		return convert.Document{}, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	defer func() { _ = file.Close() }()
	w := mdwrite.New(limits.MaxOutputBytes)
	var sections []convert.Section
	for number, sheet := range file.GetSheetList() {
		table, err := c.sheet(ctx, file, sheet, limits)
		if err != nil {
			return convert.Document{}, err
		}
		if table != "" && w.Len() > 0 {
			w.WriteString("\n\n")
		}
		start := w.Len()
		if table != "" {
			w.WriteString("## " + mdwrite.Cell(sheet) + "\n\n" + table)
		}
		sections = append(sections, convert.Section{Kind: convert.KindSheet, Number: number + 1, Name: sheet, Start: start, End: w.Len()})
	}
	if err := w.Err(); err != nil {
		return convert.Document{}, err
	}
	return convert.Document{Markdown: w.String(), Sections: sections}, nil
}

// sheet renders one sheet as a table, or "" when it is hidden or empty.
func (c *Converter) sheet(ctx context.Context, file *excelize.File, sheet string, limits convert.Limits) (string, error) {
	if visible, err := file.GetSheetVisible(sheet); err == nil && !visible && !c.opts.IncludeHidden {
		return "", nil
	}
	rows, err := file.Rows(sheet)
	if err != nil {
		return "", fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	defer func() { _ = rows.Close() }()
	table := mdwrite.NewTable(limits.MaxOutputBytes)
	for count := 0; rows.Next(); count++ {
		if count%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		row, err := rows.Columns(excelize.Options{RawCellValue: c.opts.RawValues})
		if err != nil {
			return "", fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
		}
		if err := table.Add(row); err != nil {
			return "", err
		}
	}
	if err := rows.Error(); err != nil {
		return "", fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	return table.Markdown(true), nil
}
