# Changelog

## Unreleased

- Word and PowerPoint parts nesting elements deeper than 256 levels are
  `ErrCorrupt` instead of overflowing the stack; character data is collected
  in linear time and parsing observes cancellation.
- Each parsed Word and PowerPoint XML element counts 64 bytes against
  `MaxExpandedBytes` on top of its bytes; relationship parts read before
  they are checked are bounded by the whole limit.
- Word, PowerPoint and Excel output is charged block by block and row by
  row against `MaxOutputBytes`, and spanned table cells share one escaped
  copy.
- PowerPoint chart caches are read sparsely; scatter and bubble tables no
  longer emit rows for point indices no series holds.
- PDF: `Options.MaxPages` (default 10000) and `Options.MaxPageChars` (default
  1 Mi) with the new `convert.LimitPages` and `convert.LimitPageChars`;
  characters are read one at a time with cancellation and line text is
  charged against `MaxOutputBytes` as it is read.
- HTML: block quotes and lists nested deeper than 8 levels render at the
  eighth level.

## v0.1.0

- Package `split`: structure-aware Markdown splitter with byte-range chunks,
  inherited heading paths and table headers, bounded overlap and a hard length
  limit.
- Package `convert`: `Document`, `Section`, `Input`, `Converter`, `Registry`,
  `Format` with aliases, `Limits` and the error values converters report.
- Package `convert/text`: plain text, Markdown and JSON with BOM, declared
  charset and fallback decoding.
- Package `convert/html`: HTML to CommonMark with GFM tables, `<meta>` charset
  detection, header promotion and relative link resolution.
- Packages `convert/docx` and `convert/pptx`: Word and PowerPoint documents,
  with one section per slide, chart data as tables and encrypted or legacy
  files reported as `ErrEncrypted` and `ErrUnsupported`.
- Packages `convert/csv` and `convert/xlsx`: CSV with delimiter, header and
  quoting options; Excel with one section per sheet, displayed or raw values
  and hidden sheets on request.
- Package `convert/pdf`: PDF through PDFium WebAssembly with one section per
  page, font-size headings, bounded workers and an idempotent `Close`.
- Package `convert/all`: every built-in converter in one registry with shared
  limits and fallback encoding.
