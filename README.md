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

- **Converters included.** PDF (PDFium compiled to WebAssembly, no cgo),
  Word, PowerPoint, Excel, CSV, HTML, plain text, Markdown and JSON. Pages,
  slides and sheets are recorded as byte ranges of the Markdown, so every
  chunk can be cited by page.

See [docs/design.md](docs/design.md) for the contracts and
[bench/](bench/README.md) for a comparison with langchaingo and eino-ext.

Requires Go 1.26+.

## Install

```sh
go get github.com/runforyou-ai/mdchunk
```

## Usage

```go
converters := all.New(all.Options{})
defer converters.Close()

format, _ := convert.FormatOf("report.pdf")
doc, err := converters.Convert(ctx, format, convert.Input{Reader: file})
if err != nil {
	return err // errors.Is(err, convert.ErrEncrypted), convert.ErrTooLarge, ...
}

splitter, err := split.New(split.Options{Size: 500, Overlap: 50})
if err != nil {
	return err
}
for _, chunk := range splitter.Split(doc.Markdown) {
	embedInput := chunk.TextWithContext() // "Guide > Install\n\n<chunk text>"
	pages := doc.SectionsIn(chunk.Start, chunk.End)
	store(chunk.Text, embedInput, pages)
}
```

`convert/all` pulls in every converter's dependencies. To keep a binary
small, register only what you need:

```go
var converters convert.Registry
converters.Register(text.New(text.Options{}), convert.Text, convert.Markdown)
converters.Register(docx.New(docx.Options{}), convert.DOCX)
```

Every converter's `Options` embeds `convert.Limits` (source, decompressed and
output bytes); text formats accept a `Fallback` encoding such as GB18030 for
input that is not valid UTF-8.

`Options.MaxSize` defaults to `Size + Size/5`: a section slightly longer than
`Size` stays whole instead of leaving a small tail.

## Why another splitter

General-purpose splitters either ignore Markdown structure or re-render it,
and most split CJK text only at whitespace, which it rarely has. mdchunk scans
Markdown line by line, keeps the original bytes, and ranks boundaries so that
Chinese and English prose both break at sentences.

## License

[MIT](LICENSE)
