package pdf_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rc4"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runforyou-ai/mdchunk/convert"
	"github.com/runforyou-ai/mdchunk/convert/pdf"
)

// text is a line drawn at y with font size.
type text struct {
	size float64
	y    float64
	s    string
}

// build writes a PDF with one page per entry, using Helvetica. A non-empty
// userPassword adds an RC4 standard security handler that requires it.
func build(t testing.TB, pages [][]text, userPassword string) []byte {
	t.Helper()
	var objects []string
	add := func(body string) int {
		objects = append(objects, body)
		return len(objects)
	}
	catalog := add("") // filled once the pages object exists
	pagesID := add("")
	font := add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	var kids []string
	for _, lines := range pages {
		var content strings.Builder
		for _, l := range lines {
			fmt.Fprintf(&content, "BT /F1 %g Tf 72 %g Td (%s) Tj ET\n", l.size, l.y, l.s)
		}
		stream := add(fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()))
		page := add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>", pagesID, font, stream))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	objects[catalog-1] = fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pagesID)
	objects[pagesID-1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids))
	trailer := fmt.Sprintf("/Root %d 0 R", catalog)
	if userPassword != "" {
		id := "0123456789abcdef"
		o, u := securityValues(userPassword, "owner", id, -4)
		encrypt := add(fmt.Sprintf("<< /Filter /Standard /V 1 /R 2 /O <%x> /U <%x> /P -4 >>", o, u))
		trailer += fmt.Sprintf(" /Encrypt %d 0 R /ID [<%x> <%x>]", encrypt, id, id)
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d %s >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, trailer, xref)
	return out.Bytes()
}

// padding is the password padding of the standard security handler.
var padding = []byte{0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A}

// securityValues computes the /O and /U entries for revision 2 RC4 encryption.
func securityValues(user, owner, id string, permissions int32) ([]byte, []byte) {
	pad := func(password string) []byte {
		return append([]byte(password), padding...)[:32]
	}
	crypt := func(key, data []byte) []byte {
		cipher, _ := rc4.NewCipher(key)
		out := make([]byte, len(data))
		cipher.XORKeyStream(out, data)
		return out
	}
	ownerKey := md5.Sum(pad(owner))
	o := crypt(ownerKey[:5], pad(user))
	p := uint32(permissions)
	seed := append(append(pad(user), o...), byte(p), byte(p>>8), byte(p>>16), byte(p>>24))
	key := md5.Sum(append(seed, id...))
	return o, crypt(key[:5], padding)
}

// converter is shared so PDFium starts once for the package's tests.
var converter = pdf.New(pdf.Options{})

// run converts data with the shared converter.
func run(t *testing.T, data []byte) (convert.Document, error) {
	t.Helper()
	return converter.Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)})
}

func TestPages(t *testing.T) {
	data := build(t, [][]text{
		{{24, 700, "Annual Report"}, {12, 660, "Revenue grew in every region this year,"}, {12, 646, "led by strong demand."}, {12, 600, "- not a list"}},
		{},
		{{18, 700, "Outlook"}, {12, 660, "Next year looks steady."}},
	}, "")
	doc, err := run(t, data)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Annual Report\n\nRevenue grew in every region this year,\nled by strong demand.\n\n\\- not a list\n\n## Outlook\n\nNext year looks steady."
	if doc.Markdown != want {
		t.Fatalf("got:\n%s\nwant:\n%s", doc.Markdown, want)
	}
	if len(doc.Sections) != 3 {
		t.Fatalf("sections = %+v", doc.Sections)
	}
	for i, s := range doc.Sections {
		if s.Kind != convert.KindPage || s.Number != i+1 {
			t.Errorf("section %d = %+v", i, s)
		}
	}
	first, empty, third := doc.Sections[0], doc.Sections[1], doc.Sections[2]
	if !strings.HasPrefix(doc.Markdown[first.Start:first.End], "# Annual") || !strings.HasSuffix(doc.Markdown[first.Start:first.End], "a list") {
		t.Errorf("first page = %q", doc.Markdown[first.Start:first.End])
	}
	if empty.Start != empty.End || empty.Start != third.Start {
		t.Errorf("empty page = %+v, third = %+v", empty, third)
	}
	if doc.Markdown[third.Start:third.End] != "## Outlook\n\nNext year looks steady." {
		t.Errorf("third page = %q", doc.Markdown[third.Start:third.End])
	}
}

func TestNoHeadingsAndNoText(t *testing.T) {
	c := pdf.New(pdf.Options{NoHeadings: true, Workers: 1})
	defer func() { _ = c.Close() }()
	doc, err := c.Convert(context.Background(), convert.Input{Reader: bytes.NewReader(build(t, [][]text{{{24, 700, "Title"}, {12, 650, "Body text here."}}}, ""))})
	if err != nil || doc.Markdown != "Title\n\nBody text here." {
		t.Errorf("got %q, %v", doc.Markdown, err)
	}
	doc, err = run(t, build(t, [][]text{{}, {}}, ""))
	if err != nil || doc.Markdown != "" || len(doc.Sections) != 2 || doc.Sections[1].Start != 0 {
		t.Errorf("no text: %+v, %v", doc, err)
	}
}

func TestErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"empty":     {nil, convert.ErrCorrupt},
		"garbage":   {[]byte("%PDF-1.4 not really"), convert.ErrCorrupt},
		"encrypted": {build(t, [][]text{{{12, 700, "secret"}}}, "user"), convert.ErrEncrypted},
	} {
		if _, err := run(t, tc.data); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
	_, err := pdf.New(pdf.Options{Limits: convert.Limits{MaxOutputBytes: 5}}).Convert(context.Background(),
		convert.Input{Reader: bytes.NewReader(build(t, [][]text{{{12, 700, "more than five"}}}, ""))})
	if !errors.Is(err, convert.ErrTooLarge) {
		t.Errorf("output limit: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	data := build(t, [][]text{{{12, 700, "hello"}}}, "")
	c := pdf.New(pdf.Options{Workers: 1})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if doc, err := c.Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)}); err != nil || doc.Markdown != "hello" {
				t.Errorf("concurrent convert: %q, %v", doc.Markdown, err)
			}
		})
	}
	wg.Wait()

	// A call waiting for the only worker honours its context.
	blocker := &slowReader{release: make(chan struct{}), data: data}
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = c.Convert(context.Background(), convert.Input{Reader: blocker})
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Convert(ctx, convert.Input{Reader: bytes.NewReader(data)}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting call: %v", err)
	}

	// Close waits for the call in flight and rejects waiting and later calls.
	waiting := make(chan error, 1)
	go func() {
		_, err := c.Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)})
		waiting <- err
	}()
	time.Sleep(50 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := <-waiting; !errors.Is(err, convert.ErrClosed) {
		t.Errorf("waiting call after Close: %v", err)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before the call in flight finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(blocker.release)
	if err := <-closed; err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := c.Convert(context.Background(), convert.Input{Reader: bytes.NewReader(data)}); !errors.Is(err, convert.ErrClosed) {
		t.Errorf("after Close: %v", err)
	}
}

// slowReader blocks its first read until release is closed.
type slowReader struct {
	release chan struct{}
	data    []byte
	read    bool
}

func (r *slowReader) Read(p []byte) (int, error) {
	if !r.read {
		<-r.release
		r.read = true
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}
