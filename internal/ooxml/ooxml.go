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
	"math"
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

// Node is an XML element with local names; Text is its direct character data.
type Node struct {
	Name     string
	Attrs    map[string]string
	Children []*Node
	Text     string
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
	return n.Attrs[name]
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
	archive   *zip.Reader
	max       int64
	remaining int64
}

// Open opens data as an OOXML package. maxExpanded < 0 means unlimited.
// Encrypted documents return convert.ErrEncrypted, legacy binary Office files
// convert.ErrUnsupported and anything else that is not a zip convert.ErrCorrupt.
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
	return &Package{archive: archive, max: maxExpanded, remaining: maxExpanded}, nil
}

// Files returns the package entries.
func (p *Package) Files() []*zip.File {
	return p.archive.File
}

// ReadPart parses the XML part at name, or returns nil when it is missing.
// Reads count against the expansion limit.
func (p *Package) ReadPart(name string) (*Node, error) {
	return p.readPart(name, true)
}

// readPart parses the XML part at name; counted reads count against the expansion limit.
func (p *Package) readPart(name string, counted bool) (*Node, error) {
	file, err := p.archive.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", convert.ErrCorrupt, err)
	}
	defer func() { _ = file.Close() }()
	var reader io.Reader = file
	var limited *io.LimitedReader
	if counted && p.max >= 0 && p.remaining < math.MaxInt64 {
		limited = &io.LimitedReader{R: file, N: p.remaining + 1}
		reader = limited
		defer func() { p.remaining = max(limited.N-1, 0) }()
	}
	decoder := xml.NewDecoder(reader)
	root := &Node{}
	stack := []*Node{root}
	for {
		token, err := decoder.Token()
		if limited != nil && limited.N == 0 {
			return nil, &convert.LimitError{Limit: convert.LimitExpanded, Max: p.max}
		}
		if errors.Is(err, io.EOF) {
			return root, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", convert.ErrCorrupt, name, err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			node := &Node{Name: token.Name.Local, Attrs: make(map[string]string, len(token.Attr))}
			for _, attr := range token.Attr {
				key := attr.Name.Local
				if relationshipNamespaces[attr.Name.Space] {
					key = "r:" + key
				}
				node.Attrs[key] = attr.Value
			}
			parent := stack[len(stack)-1]
			parent.Children = append(parent.Children, node)
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			stack[len(stack)-1].Text += string(token)
		}
	}
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
	var reader io.Reader = file
	var limited *io.LimitedReader
	if p.max >= 0 && p.remaining < math.MaxInt64 {
		limited = &io.LimitedReader{R: file, N: p.remaining + 1}
		reader = limited
		defer func() { p.remaining = max(limited.N-1, 0) }()
	}
	decoder := xml.NewDecoder(reader)
	depth, roots := 0, 0
	for count := 0; ; count++ {
		if count%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		token, err := decoder.Token()
		if limited != nil && limited.N == 0 {
			return &convert.LimitError{Limit: convert.LimitExpanded, Max: p.max}
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

// XMLParts returns the parts to check as XML: those [Content_Types].xml
// declares as XML, by part name or extension, targets of relationships whose
// content readers parse as XML, and other relationship targets whose content
// starts like XML. Targets are resolved as readers resolve them, with
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
	types, err := p.readPart(contentTypes, false)
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
	// Relationship targets are found from relationship parts read without counting;
	// those parts are XML and are checked, and counted, like any other.
	targets := map[string]bool{} // target -> always checked
	for _, entry := range p.archive.File {
		if !strings.HasSuffix(entry.Name, ".rels") {
			continue
		}
		root, err := p.readPart(entry.Name, false)
		if err != nil {
			return nil, err
		}
		base := path.Dir(path.Dir(entry.Name))
		for _, item := range root.Child("Relationships").Elements() {
			if item.Attr("TargetMode") == "External" {
				continue
			}
			target := strings.ReplaceAll(item.Attr("Target"), `\`, "/")
			if strings.HasPrefix(target, "/") {
				target = strings.TrimPrefix(path.Clean(target), "/")
			} else {
				target = path.Join(base, target)
			}
			targets[target] = targets[target] || xmlRelationships[path.Base(item.Attr("Type"))]
		}
	}
	var names []string
	for _, entry := range p.archive.File {
		if entry.Name == contentTypes || strings.HasSuffix(entry.Name, "/") {
			continue
		}
		always, targeted := targets[strings.ReplaceAll(entry.Name, `\`, "/")]
		if declared(entry.Name) || strings.HasSuffix(entry.Name, ".rels") || always || targeted && looksLikeXML(entry) {
			names = append(names, entry.Name)
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
func (p *Package) RequirePart(name string) (*Node, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: missing relationship target", convert.ErrCorrupt)
	}
	root, err := p.ReadPart(name)
	if err == nil && root == nil {
		return nil, fmt.Errorf("%w: missing part %s", convert.ErrCorrupt, name)
	}
	return root, err
}

// Relationships reads the relationships of part. Internal targets resolve against
// the part's directory, or the package root when they start with a slash.
func (p *Package) Relationships(part string) (map[string]Relationship, error) {
	directory, name := path.Split(part)
	root, err := p.ReadPart(directory + "_rels/" + name + ".rels")
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
