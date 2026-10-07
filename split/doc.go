// Package split splits Markdown into chunks for retrieval.
//
// Chunks are contiguous byte ranges of the input, cut preferably at block
// boundaries, then line breaks, sentence ends, clauses and whitespace, so CJK
// and Latin text both split at natural points. Standalone headings are hard
// boundaries. Each chunk carries the heading path and table header it starts
// under, with their byte ranges, so callers can add context for embedding and
// trace every piece back to the input.
//
// The splitter scans lines rather than parsing CommonMark. It recognises ATX
// and setext headings, fenced and indented code, GFM tables, HTML blocks, YAML
// front matter, list items, blockquotes, thematic breaks and inline code
// spans; see docs/design.md in the repository for the exact rules.
package split
