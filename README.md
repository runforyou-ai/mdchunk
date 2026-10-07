# mdchunk

[![CI](https://github.com/runforyou-ai/mdchunk/actions/workflows/ci.yml/badge.svg)](https://github.com/runforyou-ai/mdchunk/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/runforyou-ai/mdchunk.svg)](https://pkg.go.dev/github.com/runforyou-ai/mdchunk)

[中文](README.zh-CN.md)

mdchunk turns documents into Markdown chunks you can trace back to the source.
It is the data-preparation step of a retrieval pipeline: no models, no vector
stores, no framework, just Go.

- **Markdown in, chunks out.** Chunks are cut at block boundaries first, then
  line breaks, sentence ends, clauses and whitespace, for CJK and Latin text
  alike. Headings are hard boundaries; tables and code are only cut between lines.
- **Context without rewriting.** Every chunk carries the heading path and table
  header it starts under. Use them for embedding input; the chunk text itself
  is untouched.
- **Byte ranges everywhere.** `chunk.Text == input[chunk.Start:chunk.End]`,
  always, so you can highlight sources and cite exact passages.
- **Bounded.** `Size` is the target, `MaxSize` a hard limit, `Overlap` an upper
  bound. Lengths count code points.

Converters for PDF, Word, PowerPoint, Excel, CSV and HTML are in progress; see
[docs/design.md](docs/design.md).

Requires Go 1.26+.

## Install

```sh
go get github.com/runforyou-ai/mdchunk
```

## Usage

```go
splitter, err := split.New(split.Options{Size: 500, Overlap: 50})
if err != nil {
	return err
}
for _, chunk := range splitter.Split(markdown) {
	embedInput := chunk.TextWithContext() // "Guide > Install\n\n<chunk text>"
	store(chunk.Start, chunk.End, chunk.Text, embedInput)
}
```

`Options.MaxSize` defaults to `Size + Size/5`: a section slightly longer than
`Size` stays whole instead of leaving a small tail.

## Why another splitter

General-purpose splitters either ignore Markdown structure or re-render it,
and most split CJK text only at whitespace, which it rarely has. mdchunk scans
Markdown line by line, keeps the original bytes, and ranks boundaries so that
Chinese and English prose both break at sentences.

## License

[MIT](LICENSE)
