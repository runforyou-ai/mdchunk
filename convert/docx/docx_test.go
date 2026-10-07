package docx_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/convert/docx"
	"github.com/runforyou-ai/mdchunk/internal/ooxmltest"
)

const ns = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

// document builds a .docx whose body is body.
func document(t *testing.T, body string) []byte {
	t.Helper()
	return ooxmltest.Build(t, map[string]string{
		"word/document.xml": `<w:document ` + ns + `><w:body>` + body + `</w:body></w:document>`,
		"word/styles.xml": `<w:styles ` + ns + `>
<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/></w:style>
<w:style w:type="paragraph" w:styleId="H2"><w:name w:val="heading 2"/></w:style>
<w:style w:type="paragraph" w:styleId="Outline"><w:name w:val="Custom"/><w:pPr><w:outlineLvl w:val="2"/></w:pPr></w:style>
</w:styles>`,
		"word/numbering.xml": `<w:numbering ` + ns + `>
<w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="bullet"/></w:lvl></w:abstractNum>
<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num>
</w:numbering>`,
		"word/_rels/document.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="https://example.com/a" TargetMode="External"/>
</Relationships>`,
	})
}

// p builds a paragraph with optional properties.
func p(props, text string) string {
	return `<w:p><w:pPr>` + props + `</w:pPr><w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

// convertData runs the converter on data.
func convertData(t *testing.T, data []byte) (convert.Document, error) {
	t.Helper()
	return docx.New(docx.Options{}).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)})
}

func TestStructure(t *testing.T) {
	body := p(`<w:pStyle w:val="Title"/>`, "售后  政策") +
		p(`<w:pStyle w:val="H2"/>`, "退款") +
		p(`<w:pStyle w:val="Outline"/>`, "细则") +
		p(``, "# 不是标题") +
		p(`<w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr>`, "第一步") +
		p(`<w:numPr><w:ilvl w:val="1"/><w:numId w:val="1"/></w:numPr>`, "要点") +
		`<w:p><w:r><w:t>见</w:t></w:r><w:hyperlink r:id="rId1"><w:r><w:t>条款 [1]</w:t></w:r></w:hyperlink><w:r><w:tab/><w:t>完</w:t><w:br/><w:t>- 换行</w:t></w:r></w:p>` +
		`<w:sdt><w:sdtContent>` + p(``, "内容控件") + `</w:sdtContent></w:sdt>` +
		p(``, "   ")
	doc, err := convertData(t, document(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := "# 售后 政策\n\n## 退款\n\n### 细则\n\n\\# 不是标题\n\n1. 第一步\n\n  - 要点\n\n" +
		"见[条款 \\[1\\]](<https://example.com/a>) 完\n\\- 换行\n\n内容控件"
	if doc.Markdown != want {
		t.Errorf("got:\n%s\nwant:\n%s", doc.Markdown, want)
	}
	if doc.Sections != nil {
		t.Errorf("Sections = %v", doc.Sections)
	}
}

func TestTable(t *testing.T) {
	cell := func(props, text string) string {
		return `<w:tc><w:tcPr>` + props + `</w:tcPr>` + p(``, text) + `</w:tc>`
	}
	body := `<w:tbl>
<w:tr>` + cell(``, "地区") + cell(``, "首重") + cell(``, "续重") + `</w:tr>
<w:tr>` + cell(`<w:vMerge w:val="restart"/>`, "华东") + cell(`<w:gridSpan w:val="2"/>`, "8 | 2 元") + `</w:tr>
<w:tr>` + cell(`<w:vMerge/>`, "") + cell(``, "- 9") + cell(``, "3") + `</w:tr>
</w:tbl>`
	doc, err := convertData(t, document(t, body))
	if err != nil {
		t.Fatal(err)
	}
	want := "| 地区 | 首重 | 续重 |\n| --- | --- | --- |\n| 华东 | 8 \\| 2 元 | 8 \\| 2 元 |\n| 华东 | - 9 | 3 |"
	if doc.Markdown != want {
		t.Errorf("got:\n%s\nwant:\n%s", doc.Markdown, want)
	}
}

func TestErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"empty":     {nil, convert.ErrCorrupt},
		"not zip":   {[]byte("plain text"), convert.ErrCorrupt},
		"encrypted": {ooxmltest.Encrypted(), convert.ErrEncrypted},
		"legacy":    {ooxmltest.Legacy(), convert.ErrUnsupported},
		"no body":   {ooxmltest.Build(t, map[string]string{"word/document.xml": `<w:document ` + ns + `/>`}), convert.ErrCorrupt},
		"bad xml":   {ooxmltest.Build(t, map[string]string{"word/document.xml": `<w:document ` + ns + `><w:body>`}), convert.ErrCorrupt},
	} {
		if _, err := convertData(t, tc.data); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestLimits(t *testing.T) {
	data := document(t, strings.Repeat(p(``, strings.Repeat("长", 100)), 100))
	for name, tc := range map[string]struct {
		limits convert.Limits
		kind   string
	}{
		"expanded": {convert.Limits{MaxExpandedBytes: 1000}, convert.LimitExpanded},
		"output":   {convert.Limits{MaxOutputBytes: 1000}, convert.LimitOutput},
		"source":   {convert.Limits{MaxBytes: 100}, convert.LimitSource},
	} {
		_, err := docx.New(docx.Options{Limits: tc.limits}).Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)})
		var limit *convert.LimitError
		if !errors.As(err, &limit) || limit.Limit != tc.kind {
			t.Errorf("%s: got %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := docx.New(docx.Options{}).Convert(ctx, convert.Input{Reader: bytes.NewReader(data)}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func FuzzConvert(f *testing.F) {
	f.Add([]byte("PK"))
	c := docx.New(docx.Options{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = c.Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)})
	})
}
