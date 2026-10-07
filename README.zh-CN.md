# mdchunk

[![CI](https://github.com/runforyou-ai/mdchunk/actions/workflows/ci.yml/badge.svg)](https://github.com/runforyou-ai/mdchunk/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/runforyou-ai/mdchunk.svg)](https://pkg.go.dev/github.com/runforyou-ai/mdchunk)

[English](README.md)

mdchunk 把文档转换为可以追溯到原文的 Markdown 分段，是检索流程中的数据准备环节：不调用模型、不涉及向量库、不依赖任何框架，只用 Go。

- **输入 Markdown，输出分段。** 依次优先在块边界、换行、句末、分句标点和空白处切分，中文与英文都在自然位置断开。标题是硬边界，表格和代码只在行间切分。
- **补充上下文，不改写正文。** 每个分段带上它所在的标题路径和表头，可用作向量化输入；分段正文保持原样。
- **处处是字节区间。** `chunk.Text == input[chunk.Start:chunk.End]` 始终成立，可以直接高亮原文、引用准确段落。
- **长度有界。** `Size` 是目标长度，`MaxSize` 是硬上限，`Overlap` 是重叠上限，长度按 Unicode 码点计算。

PDF、Word、PowerPoint、Excel、CSV 和 HTML 转换器正在开发中，见 [docs/design.md](docs/design.md)。

需要 Go 1.26 及以上版本。

## 安装

```sh
go get github.com/runforyou-ai/mdchunk
```

## 用法

```go
splitter, err := split.New(split.Options{Size: 500, Overlap: 50})
if err != nil {
	return err
}
for _, chunk := range splitter.Split(markdown) {
	embedInput := chunk.TextWithContext() // "指南 > 安装\n\n<分段正文>"
	store(chunk.Start, chunk.End, chunk.Text, embedInput)
}
```

`Options.MaxSize` 默认为 `Size + Size/5`：略长于 `Size` 的小节保持完整，不会留下很短的尾段。

## 为什么再做一个切分器

通用切分器要么忽略 Markdown 结构，要么把它重新渲染；多数只在空白处切分中文，而中文很少有空白。mdchunk 逐行扫描 Markdown，保留原始字节，并对边界分级，使中文和英文都在句子处断开。

## 许可证

[MIT](LICENSE)
