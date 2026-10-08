# mdchunk

[![CI](https://github.com/runforyou-ai/mdchunk/actions/workflows/ci.yml/badge.svg)](https://github.com/runforyou-ai/mdchunk/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/runforyou-ai/mdchunk.svg)](https://pkg.go.dev/github.com/runforyou-ai/mdchunk)

[English](README.md)

mdchunk 把文档转换为可以追溯到原文的 Markdown 分段，是检索流程中的数据准备环节：不调用模型、不涉及向量库、不依赖任何框架，只用 Go。

- **输入 Markdown，输出分段。** 依次优先在块边界、换行、句末、分句标点和空白处切分，中文与英文都在自然位置断开。标题是硬边界，表格和代码只在行间切分。
- **补充上下文，不改写正文。** 每个分段带上它所在的标题路径和表头，可用作向量化输入；分段正文保持原样。
- **处处是字节区间。** `chunk.Text == input[chunk.Start:chunk.End]` 始终成立，可以直接高亮原文、引用准确段落。
- **长度有界。** `Size` 是目标长度，`MaxSize` 是硬上限，`Overlap` 是重叠上限，长度按 Unicode 码点计算。
- **内置转换器。** PDF（PDFium 编译为 WebAssembly，无需 cgo）、Word、PowerPoint、Excel、CSV、HTML、纯文本、Markdown 和 JSON。页、幻灯片和工作表以 Markdown 字节区间记录，每个分段都能注明出自哪一页。

契约见 [docs/design.md](docs/design.md)，与 langchaingo、eino-ext 的对比见 [bench/](bench/README.md)。

需要 Go 1.26 及以上版本。

## 安装

```sh
go get github.com/runforyou-ai/mdchunk
```

## 用法

```go
converters := all.New(all.Options{})
defer func() { _ = converters.Close() }()

format, _ := convert.FormatOf("report.pdf")
doc, err := converters.Convert(ctx, format, convert.Input{Reader: file})
if err != nil {
	return err // errors.Is(err, convert.ErrEncrypted)、convert.ErrTooLarge 等
}

splitter, err := split.New(split.Options{Size: 500, Overlap: 50})
if err != nil {
	return err
}
for _, chunk := range splitter.Split(doc.Markdown) {
	embedInput := chunk.TextWithContext() // "指南 > 安装\n\n<分段正文>"
	pages := doc.SectionsIn(chunk.Start, chunk.End)
	store(chunk.Text, embedInput, pages)
}
```

`convert/all` 会引入所有转换器的依赖。想让程序更小，只注册需要的转换器：

```go
var converters convert.Registry
converters.Register(text.New(text.Options{}), convert.Text, convert.Markdown)
converters.Register(docx.New(docx.Options{}), convert.DOCX)
```

每个转换器的 `Options` 都嵌入 `convert.Limits`（原件、解压后和输出的字节上限）；文本类格式可以设置 `Fallback` 编码（如 GB18030），用于解码不是合法 UTF-8 的输入。

`Options.MaxSize` 默认为 `Size + Size/5`：略长于 `Size` 的小节保持完整，不会留下很短的尾段。

## 为什么再做一个切分器

通用切分器要么忽略 Markdown 结构，要么把它重新渲染；多数只在空白处切分中文，而中文很少有空白。mdchunk 逐行扫描 Markdown，保留原始字节，并对边界分级，使中文和英文都在句子处断开。

## 许可证

[MIT](LICENSE)
