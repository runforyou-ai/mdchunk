# Splitter comparison

This nested module compares `split` with langchaingo's `MarkdownTextSplitter`
(heading hierarchy, code blocks and joined table rows enabled) and eino-ext's
header splitter followed by its recursive splitter (with Chinese separators
and code point lengths). It lives in its own module so the main module does
not depend on either framework.

```sh
cd bench
go run . -size 200 -overlap 30
```

- **over limit**: chunks longer than `size + size/5` code points.
- **source preserved**: every chunk is a substring of the input.

Results with the defaults:

| document | splitter | chunks | longest | over limit | source preserved |
|---|---|---|---|---|---|
| crlf | mdchunk | 2 | 85 | 0 | yes |
| crlf | langchaingo | 2 | 77 | 0 | no |
| crlf | eino-ext | 2 | 64 | 0 | no |
| en-guide | mdchunk | 12 | 209 | 0 | yes |
| en-guide | langchaingo | 12 | 252 | 1 | no |
| en-guide | eino-ext | 11 | 192 | 0 | no |
| zh-paragraph | mdchunk | 7 | 184 | 0 | yes |
| zh-paragraph | langchaingo | 1 | 1040 | 1 | yes |
| zh-paragraph | eino-ext | 7 | 184 | 0 | yes |
| zh-policy | mdchunk | 6 | 214 | 0 | yes |
| zh-policy | langchaingo | 7 | 204 | 0 | no |
| zh-policy | eino-ext | 7 | 191 | 0 | no |

langchaingo re-renders Markdown and, with its default separators, cannot cut
Chinese prose. eino-ext's header splitter trims every line and drops blank
lines, which removes code indentation, and its table chunks carry no header.
Neither reports where a chunk came from.
