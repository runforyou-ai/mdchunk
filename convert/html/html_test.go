package html_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

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
	got := run(t, convert.Input{Reader: strings.NewReader(`<table>
<tr><td>地区</td><td>运费</td></tr>
<tr><td colspan="2">合并</td></tr>
</table>`)})
	want := "| 地区 | 运费 |\n|---|---|\n| 合并 | 合并 |"
	if !strings.Contains(got, want) {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
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

func TestCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := html.New(html.Options{}).Convert(ctx, convert.Input{Reader: strings.NewReader("<p>x</p>")})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v", err)
	}
}

func FuzzConvert(f *testing.F) {
	for _, seed := range []string{"<p>x</p>", "<table><tr><td rowspan=3>a</td></tr></table>", "<meta charset=x>", "<<>>"} {
		f.Add(seed)
	}
	c := html.New(html.Options{})
	f.Fuzz(func(t *testing.T, src string) {
		doc, err := c.Convert(context.Background(), convert.Input{Reader: strings.NewReader(src)})
		if err != nil && !errors.Is(err, convert.ErrCorrupt) {
			t.Fatal(err)
		}
		if strings.ContainsAny(doc.Markdown, "\r\x00") {
			t.Fatalf("output not normalised: %q", doc.Markdown)
		}
	})
}
