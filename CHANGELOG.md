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
