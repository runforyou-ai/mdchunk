package html_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/convert/html"
)

// run converts source with default options.
func run(t *testing.T, in convert.Input) string {
	t.Helper()
	doc, err := html.New(html.Options{}).Convert(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Sections != nil {
		t.Errorf("Sections = %v", doc.Sections)
	}
	return doc.Markdown
}

// utf16 encodes s as UTF-16LE with a BOM.
func utf16(t *testing.T, s string) string {
	t.Helper()
	b, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().String(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gbk returns "<p>中文</p>" encoded as GB18030.
func gbk(t *testing.T) string {
	t.Helper()
	b, err := simplifiedchinese.GB18030.NewEncoder().String("<p>中文</p>")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStructure(t *testing.T) {
	got := run(t, convert.Input{Reader: strings.NewReader(`<html><head><title>x</title><style>p{}</style></head><body>
<h1>标题</h1><p>第一段，<a href="/docs">链接</a>。</p>
<ul><li>一</li><li>二</li></ul>
<pre><code>code</code></pre>
<script>alert(1)</script>
</body></html>`), BaseURL: "https://example.com/a/"})
	for _, want := range []string{"# 标题", "第一段，[链接](https://example.com/docs)。", "- 一\n- 二", "```\ncode\n```"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "alert") || strings.Contains(got, "p{}") {
		t.Errorf("script or style leaked:\n%s", got)
	}
}

func TestTables(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"promoted header": {`<table><tr><td>地区</td><td>运费</td></tr><tr><td colspan="2">合并</td></tr></table>`,
			"| 地区 | 运费 |\n|---|---|\n| 合并 | 合并 |"},
		"adjacent colspans": {`<table><tr><th>1</th><th>2</th><th>3</th><th>4</th><th>5</th><th>6</th></tr>
<tr><td colspan="3">A</td><td colspan="2">B</td><td>C</td></tr></table>`,
			"| 1 | 2 | 3 | 4 | 5 | 6 |\n|---|---|---|---|---|---|\n| A | A | A | B | B | C |"},
		"rowspan": {`<table><thead><tr><th>k</th><th>v</th></tr></thead><tbody>
<tr><td rowspan="2">a</td><td>1</td></tr><tr><td>2</td></tr><tr><td>b</td><td rowspan="0">3</td></tr><tr><td>c</td></tr></tbody></table>`,
			"| k | v |\n|---|---|\n| a | 1 |\n| a | 2 |\n| b | 3 |\n| c | 3 |"},
		"ragged rows": {`<table><tr><th>x</th></tr><tr><td>1</td><td>2</td></tr></table>`,
			"| x |  |\n|---|---|\n| 1 | 2 |"},
		"nested adjacent colspans": {`<table><tr><th>outer</th></tr><tr><td><table><tr><th>1</th><th>2</th><th>3</th><th>4</th><th>5</th><th>6</th></tr>
<tr><td colspan="3">A</td><td colspan="2">B</td><td>C</td></tr></table></td></tr></table>`, "| A | A | A | B | B | C |"},
		"row groups": {`<table><thead><tr><th>k</th><th>v</th></tr></thead><tbody><tr><td rowspan="0">A</td><td>1</td></tr></tbody>
<tbody><tr><td>B</td><td>2</td></tr></tbody></table>`, "| A | 1 |\n| B | 2 |"},
		"overflowing span": {`<table><tr><th>h</th></tr><tr><td colspan="99999999999999999999">x</td></tr></table>`, "| x |" + strings.Repeat(" x |", 999)},
		"inline code kinds": {`<table><tr><th>a</th><th>b</th><th>c</th><th>d</th></tr><tr><td><var>a|b</var></td><td><samp>c|d</samp></td><td><kbd>e|f</kbd></td><td><tt>g|h</tt></td></tr></table>`,
			"| `a\\|b` | `c\\|d` | `e\\|f` | `g\\|h` |"},
		"code pipe": {`<table><tr><th>c</th><th>d</th></tr><tr><td><code>a|b</code></td><td>x</td></tr></table>`,
			"| c | d |\n|---|---|\n| `a\\|b` | x |"},
	} {
		if got := run(t, convert.Input{Reader: strings.NewReader(tc.src)}); !strings.Contains(got, tc.want) {
			t.Errorf("%s: got:\n%s\nwant:\n%s", name, got, tc.want)
		}
	}
}

func TestHugeSpans(t *testing.T) {
	start := time.Now()
	src := `<table><tr><td colspan=100000000 rowspan=100000000>x</td></tr></table>`
	if got := run(t, convert.Input{Reader: strings.NewReader(src)}); strings.Count(got, "x") != 1000 {
		t.Errorf("colspan not clamped to 1000: %d cells", strings.Count(got, "x"))
	}
	_, err := html.New(html.Options{Limits: convert.Limits{MaxOutputBytes: 1000}}).Convert(context.Background(),
		convert.Input{Reader: strings.NewReader(`<table><tr><td colspan=1000>x</td></tr>` + strings.Repeat("<tr><td>y</td></tr>", 100) + `</table>`)})
	var limit *convert.LimitError
	if !errors.As(err, &limit) || limit.Limit != convert.LimitOutput || limit.Max != 1000 {
		t.Errorf("cell budget: %v", err)
	}
	_, err = html.New(html.Options{Limits: convert.Limits{MaxOutputBytes: 3000}}).Convert(context.Background(),
		convert.Input{Reader: strings.NewReader(`<table><tr><td colspan=1000>` + strings.Repeat("<b>x</b>", 128) + `</td></tr></table>`)})
	if !errors.As(err, &limit) || limit.Limit != convert.LimitOutput {
		t.Errorf("content budget: %v", err)
	}
	_, err = html.New(html.Options{Limits: convert.Limits{MaxOutputBytes: 12000}}).Convert(context.Background(),
		convert.Input{Reader: strings.NewReader(`<table><tr><td colspan=1000 rowspan=2>x</td></tr><tr><td rowspan=1000>y</td></tr>` + strings.Repeat("<tr></tr>", 999) + `</table>`)})
	if !errors.As(err, &limit) {
		t.Errorf("sparse rows: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("huge spans took %v", elapsed)
	}
}

func TestCharset(t *testing.T) {
	body := "<p>中文</p>"
	for name, in := range map[string]convert.Input{
		"meta charset":   {Reader: strings.NewReader(`<meta charset="gbk">` + gbk(t))},
		"http-equiv":     {Reader: strings.NewReader(`<meta http-equiv="Content-Type" content="text/html; charset=gb2312">` + gbk(t))},
		"input charset":  {Reader: strings.NewReader(gbk(t)), Charset: "gb18030"},
		"charset wins":   {Reader: strings.NewReader(`<meta charset="utf-8">` + gbk(t)), Charset: "gbk"},
		"meta in body":   {Reader: strings.NewReader("<body>" + body + `<meta charset="gbk"></body>`)},
		"utf-8 with bom": {Reader: strings.NewReader("\xEF\xBB\xBF" + body)},
		"utf-16 bom":     {Reader: strings.NewReader(utf16(t, body))},
		"unknown input":  {Reader: strings.NewReader(`<meta charset="gbk">` + gbk(t)), Charset: "bogus"},
	} {
		if got := run(t, in); got != "中文" {
			t.Errorf("%s: got %q", name, got)
		}
	}
	doc, err := html.New(html.Options{Fallback: simplifiedchinese.GB18030}).Convert(context.Background(),
		convert.Input{Reader: strings.NewReader(gbk(t))})
	if err != nil || doc.Markdown != "中文" {
		t.Errorf("fallback: %q, %v", doc.Markdown, err)
	}
}

func TestEmptyAndLimits(t *testing.T) {
	for _, src := range []string{"", "  \n", "<html><body></body></html>"} {
		if got := run(t, convert.Input{Reader: strings.NewReader(src)}); got != "" {
			t.Errorf("%q gave %q", src, got)
		}
	}
	_, err := html.New(html.Options{Limits: convert.Limits{MaxOutputBytes: 5}}).Convert(context.Background(),
		convert.Input{Reader: strings.NewReader("<p>more than five bytes</p>")})
	var limit *convert.LimitError
	if !errors.As(err, &limit) || limit.Limit != convert.LimitOutput {
		t.Errorf("output limit: %v", err)
	}
	_, err = html.New(html.Options{Limits: convert.Limits{MaxBytes: 5}}).Convert(context.Background(),
		convert.Input{Reader: strings.NewReader("<p>more than five bytes</p>")})
	if !errors.As(err, &limit) || limit.Limit != convert.LimitSource {
		t.Errorf("source limit: %v", err)
	}
}

// cancelOnEOF cancels a context when its reader is drained.
type cancelOnEOF struct {
	r      io.Reader
	cancel func()
}

func (c cancelOnEOF) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) {
		c.cancel()
	}
	return n, err
}

func TestCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := html.New(html.Options{}).Convert(ctx, convert.Input{Reader: strings.NewReader("<p>x</p>")})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("before reading: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	doc, err := html.New(html.Options{}).Convert(ctx, convert.Input{Reader: cancelOnEOF{strings.NewReader("<p>x</p>"), cancel}})
	if !errors.Is(err, context.Canceled) || doc.Markdown != "" {
		t.Errorf("during conversion: %q, %v", doc.Markdown, err)
	}
}

func FuzzConvert(f *testing.F) {
	for _, seed := range []string{"<p>x</p>", "<table><tr><td rowspan=3>a</td></tr></table>", "<meta charset=x>", "<<>>"} {
		f.Add(seed)
	}
	c := html.New(html.Options{Limits: convert.Limits{MaxOutputBytes: 1 << 20}})
	f.Fuzz(func(t *testing.T, src string) {
		doc, err := c.Convert(context.Background(), convert.Input{Reader: strings.NewReader(src)})
		if err != nil && !errors.Is(err, convert.ErrCorrupt) && !errors.Is(err, convert.ErrTooLarge) {
			t.Fatal(err)
		}
		if err != nil && (doc.Markdown != "" || doc.Sections != nil) {
			t.Fatalf("partial document on error: %q", doc.Markdown)
		}
		if strings.ContainsAny(doc.Markdown, "\r\x00") {
			t.Fatalf("output not normalised: %q", doc.Markdown)
		}
	})
}
