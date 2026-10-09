// Package ooxml reads Office Open XML packages under the expansion limit.
package ooxml

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/runforyou-ai/mdchunk/convert"
)

// relationshipNamespaces qualify relationship attributes in Transitional and
// Strict documents; such attributes are keyed with an "r:" prefix.
var relationshipNamespaces = map[string]bool{
	"http://schemas.openxmlformats.org/officeDocument/2006/relationships": true,
	"http://purl.oclc.org/ooxml/officeDocument/relationships":             true,
}

// utf8BOM may start an XML part.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// oleMagic starts a Compound File Binary, used by encrypted OOXML and legacy Office formats.
var oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// encryptionInfo is the UTF-16LE name of the stream an encrypted OOXML container holds.
var encryptionInfo = []byte("E\x00n\x00c\x00r\x00y\x00p\x00t\x00i\x00o\x00n\x00I\x00n\x00f\x00o\x00")

// MaxDepth is the deepest element nesting a part may have; deeper parts are
// reported as convert.ErrCorrupt. Office applications nest far less.
const MaxDepth = 256

// ElementCost is what each element of a parsed part counts against the
// expansion limit in addition to its bytes. A parsed element takes about
// twice that, so the parsed tree stays within about twice the limit.
const ElementCost = 64

// Node is an XML element with local names; Text is its direct character data.
type Node struct {
	Name     string
	attrs    []attr
	Children []*Node
	Text     string
}

// attr is an attribute keyed by its local name, or "r:" and its local name
// for relationship attributes.
type attr struct {
	key, value string
}

// Child returns the first child named name, or nil.
func (n *Node) Child(name string) *Node {
	if n == nil {
		return nil
	}
	for _, child := range n.Children {
		if child.Name == name {
			return child
		}
	}
	return nil
}

// Elements returns the children, or nil for a nil node.
func (n *Node) Elements() []*Node {
	if n == nil {
		return nil
	}
	return n.Children
}

// Attr returns an attribute value, or "" for a nil node.
func (n *Node) Attr(name string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.attrs {
		if a.key == name {
			return a.value
		}
	}
	return ""
}

// Relationship is a part relationship; Type is the last segment of the type URI and
// internal targets are absolute paths within the package.
type Relationship struct {
	Type     string
	Target   string
	External bool
}

// Package is an open OOXML package.
type Package struct {
	archive *zip.Reader
	max     int64
	budget  *budget // nil when unlimited
}

// budget is what is left of an expansion limit.
type budget struct {
	max, left int64
}

// spend charges n against b, returning an expanded *convert.LimitError once
// more than the limit has been spent. A nil budget is unlimited.
func (b *budget) spend(n int64) error {
	if b == nil {
		return nil
	}
	if n > b.left {
		b.left = -1
		return &convert.LimitError{Limit: convert.LimitExpanded, Max: b.max}
	}
	b.left -= n
	return nil
}

// meteredReader charges the bytes it reads against a budget and keeps the
// first limit error.
type meteredReader struct {
	r      io.Reader
	budget *budget
	err    error
}

// Read reads from the underlying reader and charges what it returns.
func (m *meteredReader) Read(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	n, err := m.r.Read(p)
	if spendErr := m.budget.spend(int64(n)); spendErr != nil {
		m.err = spendErr
		return n, spendErr
	}
	return n, err
}

// Open opens data as an OOXML package. maxExpanded < 0 means unlimited.
// Encrypted documents return convert.ErrEncrypted, legacy binary Office files
// convert.ErrUnsupported, and anything else that is not a zip, or holds two
// entries whose names differ only in case or separators, convert.ErrCorrupt.
func Open(data []byte, maxExpanded int64) (*Package, error) {
	if bytes.HasPrefix(data, oleMagic) {
		if bytes.Contains(data, encryptionInfo) {
			return nil, convert.ErrEncrypted
		}
		return nil, fmt.Errorf("%w: legacy binary Office format", convert.ErrUnsupported)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	// Readers disagree on which of two same-named entries wins, so duplicates are damage.
	names := map[string]bool{}
	for _, entry := range archive.File {
		name := strings.ToLower(strings.ReplaceAll(entry.Name, `\`, "/"))
		if names[name] {
			return nil, fmt.Errorf("%w: duplicate part %s", convert.ErrCorrupt, entry.Name)
		}
		names[name] = true
	}
	pkg := &Package{archive: archive, max: maxExpanded}
	if maxExpanded >= 0 {
		pkg.budget = &budget{max: maxExpanded, left: maxExpanded}
	}
	return pkg, nil
}

// Files returns the package entries.
func (p *Package) Files() []*zip.File {
	return p.archive.File
}

// ReadPart parses the XML part at name, or returns nil when it is missing.
// Its bytes and elements count against the expansion limit; cancellation is
// checked between tokens in batches.
func (p *Package) ReadPart(ctx context.Context, name string) (*Node, error) {
	return p.readPart(ctx, name, p.budget)
}

// uncounted returns a budget of the whole expansion limit for a read that is
// counted again later, or nil when the package is unlimited.
func (p *Package) uncounted() *budget {
	if p.budget == nil {
		return nil
	}
	return &budget{max: p.max, left: p.max}
}

// readPart parses the XML part at name, charging its bytes and elements to b.
// Parts nesting elements deeper than MaxDepth return convert.ErrCorrupt.
func (p *Package) readPart(ctx context.Context, name string, b *budget) (*Node, error) {
	file, err := p.archive.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	defer func() { _ = file.Close() }()
	reader := &meteredReader{r: file, budget: b}
	decoder := xml.NewDecoder(reader)
	root := &Node{}
	// texts[i] collects the character data of stack[i] until the element ends.
	stack, texts := []*Node{root}, [][]byte{nil}
	for count := 0; ; count++ {
		if count%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		token, err := decoder.Token()
		if reader.err != nil {
			return nil, reader.err
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", convert.ErrCorrupt, name, err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if len(stack) > MaxDepth {
				return nil, fmt.Errorf("%w: %s nests elements deeper than %d", convert.ErrCorrupt, name, MaxDepth)
			}
			if err := b.spend(ElementCost); err != nil {
				return nil, err
			}
			node := &Node{Name: token.Name.Local}
			if len(token.Attr) > 0 {
				node.attrs = make([]attr, len(token.Attr))
			}
			for i, a := range token.Attr {
				key := a.Name.Local
				if relationshipNamespaces[a.Name.Space] {
					key = "r:" + key
				}
				node.attrs[i] = attr{key: key, value: a.Value}
			}
			parent := stack[len(stack)-1]
			parent.Children = append(parent.Children, node)
			stack = append(stack, node)
			if len(texts) < len(stack) {
				texts = append(texts, nil)
			}
			texts[len(stack)-1] = texts[len(stack)-1][:0]
		case xml.EndElement:
			if len(stack) > 1 {
				top := len(stack) - 1
				stack[top].Text = string(texts[top])
				stack = stack[:top]
			}
		case xml.CharData:
			top := len(stack) - 1
			texts[top] = append(texts[top], token...)
		}
	}
	for i, node := range stack {
		node.Text = string(texts[i])
	}
	return root, nil
}

// CheckXML reads the XML part at name to its end without keeping it and
// returns convert.ErrCorrupt when it is not well-formed: mismatched or
// unclosed elements, other than one root element, or text outside it. Reads count against
// the expansion limit; cancellation is checked between tokens in batches.
func (p *Package) CheckXML(ctx context.Context, name string) error {
	file, err := p.archive.Open(name)
	if err != nil {
		return fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	defer func() { _ = file.Close() }()
	reader := &meteredReader{r: file, budget: p.budget}
	decoder := xml.NewDecoder(reader)
	depth, roots := 0, 0
	for count := 0; ; count++ {
		if count%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		token, err := decoder.Token()
		if reader.err != nil {
			return reader.err
		}
		if errors.Is(err, io.EOF) {
			if roots != 1 {
				return fmt.Errorf("%w: %s has %d root elements", convert.ErrCorrupt, name, roots)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %s: %w", convert.ErrCorrupt, name, err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && roots == 0 {
				token = bytes.TrimPrefix(token, utf8BOM)
			}
			if depth == 0 && len(bytes.TrimSpace(token)) > 0 {
				return fmt.Errorf("%w: %s has text outside its root element", convert.ErrCorrupt, name)
			}
		}
	}
}

// xmlRelationships are relationship types whose targets readers parse as
// XML content; their targets are always checked.
var xmlRelationships = map[string]bool{
	"officeDocument": true, "worksheet": true, "sharedStrings": true, "styles": true, "theme": true,
}

// caseInsensitiveParts are parts readers find whatever the case of their name.
var caseInsensitiveParts = map[string]bool{"xl/sharedstrings.xml": true}

// fixedXMLParts are parts readers load from fixed paths, compared case-insensitively;
// they are always checked when present.
var fixedXMLParts = map[string]bool{
	"xl/workbook.xml": true, "xl/sharedstrings.xml": true, "xl/styles.xml": true, "xl/theme/theme1.xml": true,
	"word/document.xml": true, "word/styles.xml": true, "word/numbering.xml": true, "ppt/presentation.xml": true,
}

// XMLParts returns the parts to check as XML: those [Content_Types].xml
// declares as XML, by part name or extension, parts readers load from fixed
// paths, targets of relationships whose content readers parse as XML, and
// other relationship targets whose content starts like XML. The check guards
// against damaged files; a package crafted so that a reader resolves a part
// differently can still convert to less text, as a file holding less text
// would. Targets are resolved as readers resolve them, with
// backslashes as slashes and dot segments removed. [Content_Types].xml is
// checked strictly here and counted once; parts that cannot be opened are left
// to the reader that needs them.
func (p *Package) XMLParts(ctx context.Context) ([]string, error) {
	const contentTypes = "[Content_Types].xml"
	if file, err := p.archive.Open(contentTypes); err == nil {
		_ = file.Close()
		if err := p.CheckXML(ctx, contentTypes); err != nil {
			return nil, err
		}
	}
	types, err := p.readPart(ctx, contentTypes, p.uncounted())
	if err != nil {
		return nil, err
	}
	isXML := func(contentType string) bool {
		contentType = strings.ToLower(strings.TrimSpace(contentType))
		return strings.HasSuffix(contentType, "+xml") || strings.HasSuffix(contentType, "/xml")
	}
	defaults, overrides := map[string]bool{}, map[string]bool{}
	for _, item := range types.Child("Types").Elements() {
		switch item.Name {
		case "Default":
			defaults[strings.ToLower(item.Attr("Extension"))] = isXML(item.Attr("ContentType"))
		case "Override":
			overrides[strings.ToLower(strings.TrimPrefix(item.Attr("PartName"), "/"))] = isXML(item.Attr("ContentType"))
		}
	}
	declared := func(name string) bool {
		lower := strings.ToLower(name)
		if xmlPart, ok := overrides[lower]; ok {
			return xmlPart
		}
		return defaults[strings.TrimPrefix(path.Ext(lower), ".")]
	}
	// Relationship targets are found from relationship parts read under a budget of
	// their own; those parts are XML and are checked, and counted, like any other.
	targets := map[string]bool{} // target -> always checked
	var required [][2]string     // both resolutions of each core relationship target
	for _, entry := range p.archive.File {
		name := strings.ReplaceAll(entry.Name, `\`, "/")
		if !strings.HasSuffix(name, ".rels") {
			continue
		}
		root, err := p.readPart(ctx, name, p.uncounted())
		if err != nil {
			return nil, err
		}
		base := path.Dir(path.Dir(name))
		for _, item := range root.Child("Relationships").Elements() {
			if item.Attr("TargetMode") == "External" {
				continue
			}
			// Readers differ in whether backslashes become slashes before or after dot
			// segments are removed, so both resolutions are recorded.
			raw := item.Attr("Target")
			always := xmlRelationships[path.Base(item.Attr("Type"))]
			cleaned := strings.ReplaceAll(raw, `\`, "/")
			if strings.HasPrefix(cleaned, "/") {
				cleaned = strings.TrimPrefix(path.Clean(cleaned), "/")
			} else {
				cleaned = path.Join(base, cleaned)
			}
			literal := strings.ReplaceAll(path.Clean(raw), `\`, "/")
			if strings.HasPrefix(literal, "/") {
				literal = strings.TrimPrefix(literal, "/")
			} else if base != "." {
				literal = base + "/" + literal
			}
			for _, target := range []string{cleaned, literal} {
				targets[target] = targets[target] || always
			}
			if always {
				required = append(required, [2]string{cleaned, literal})
			}
		}
	}
	// A core part a relationship names must exist as a file under one of its
	// resolutions; shared strings are matched without regard to case, as
	// readers match them.
	present, folded := map[string]bool{}, map[string]bool{}
	for _, entry := range p.archive.File {
		normalized := strings.ReplaceAll(entry.Name, `\`, "/")
		if strings.HasSuffix(normalized, "/") {
			continue
		}
		present[normalized], folded[strings.ToLower(normalized)] = true, true
	}
	exists := func(name string) bool {
		return present[name] || caseInsensitiveParts[strings.ToLower(name)] && folded[strings.ToLower(name)]
	}
	for _, target := range required {
		if !exists(target[0]) && !exists(target[1]) {
			return nil, fmt.Errorf("%w: missing part %s", convert.ErrCorrupt, target[0])
		}
	}
	var names []string
	for _, entry := range p.archive.File {
		normalized := strings.ReplaceAll(entry.Name, `\`, "/")
		if normalized == contentTypes || strings.HasSuffix(normalized, "/") {
			continue
		}
		always, targeted := targets[normalized]
		if !targeted {
			always, targeted = targets[path.Clean(normalized)]
		}
		always = always || fixedXMLParts[strings.ToLower(normalized)]
		if declared(entry.Name) || strings.HasSuffix(entry.Name, ".rels") || always || targeted && looksLikeXML(entry) {
			names = append(names, normalized)
		}
	}
	return names, nil
}

// looksLikeXML reports whether an entry's content starts with "<" after an
// optional BOM and any amount of whitespace.
func looksLikeXML(entry *zip.File) bool {
	file, err := entry.Open()
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	if head, err := reader.Peek(len(utf8BOM)); err == nil && bytes.Equal(head, utf8BOM) {
		_, _ = reader.Discard(len(utf8BOM))
	}
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return false
		}
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return b == '<'
	}
}

// RequirePart is ReadPart for a part the document cannot do without; a missing
// part returns convert.ErrCorrupt.
func (p *Package) RequirePart(ctx context.Context, name string) (*Node, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: missing relationship target", convert.ErrCorrupt)
	}
	root, err := p.ReadPart(ctx, name)
	if err == nil && root == nil {
		return nil, fmt.Errorf("%w: missing part %s", convert.ErrCorrupt, name)
	}
	return root, err
}

// Relationships reads the relationships of part. Internal targets resolve against
// the part's directory, or the package root when they start with a slash.
func (p *Package) Relationships(ctx context.Context, part string) (map[string]Relationship, error) {
	directory, name := path.Split(part)
	root, err := p.ReadPart(ctx, directory+"_rels/"+name+".rels")
	if err != nil {
		return nil, err
	}
	relationships := map[string]Relationship{}
	for _, item := range root.Child("Relationships").Elements() {
		target, external := item.Attr("Target"), item.Attr("TargetMode") == "External"
		if !external {
			if strings.HasPrefix(target, "/") {
				target = strings.TrimPrefix(target, "/")
			} else {
				target = path.Join(directory, target)
			}
		}
		relationships[item.Attr("Id")] = Relationship{Type: path.Base(item.Attr("Type")), Target: target, External: external}
	}
	return relationships, nil
}
