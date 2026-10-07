# Changelog

## Unreleased

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
