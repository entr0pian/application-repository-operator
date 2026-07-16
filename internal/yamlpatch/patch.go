// Package yamlpatch mutates specific nested keys in a YAML document while
// preserving everything else — comments, key order, and unrelated sibling
// content. It has no I/O; callers are responsible for fetching and writing
// back the raw bytes.
package yamlpatch

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Document wraps a parsed YAML document for structure-preserving edits.
type Document struct {
	doc *yaml.Node // Kind == yaml.DocumentNode
}

// Parse parses content into a Document. Empty content is treated as an
// empty mapping document, so callers can patch a brand new file the same
// way they patch an existing one.
func Parse(content []byte) (*Document, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(content)) > 0 {
		if err := yaml.Unmarshal(content, &doc); err != nil {
			return nil, fmt.Errorf("yamlpatch: parse: %w", err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{newMapping()}}
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{newMapping()}
	}
	return &Document{doc: &doc}, nil
}

func newMapping() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

func (d *Document) root() *yaml.Node {
	return d.doc.Content[0]
}

// GetString returns the scalar string at path and whether it was present.
func (d *Document) GetString(path ...string) (string, bool) {
	n := d.find(d.root(), path, false)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

// GetBool returns the scalar bool at path and whether it was present.
func (d *Document) GetBool(path ...string) (bool, bool) {
	s, ok := d.GetString(path...)
	if !ok {
		return false, false
	}
	return s == "true", true
}

// SetString sets a scalar string value at path, creating intermediate
// mapping nodes as needed. Returns true if the value actually changed.
func (d *Document) SetString(value string, path ...string) bool {
	n := d.find(d.root(), path, true)
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Value == value {
		return false
	}
	n.Kind, n.Tag, n.Value, n.Content = yaml.ScalarNode, "!!str", value, nil
	return true
}

// SetBool sets a scalar bool value at path, creating intermediate mapping
// nodes as needed. Returns true if the value actually changed.
func (d *Document) SetBool(value bool, path ...string) bool {
	s := "false"
	if value {
		s = "true"
	}
	n := d.find(d.root(), path, true)
	if n.Kind == yaml.ScalarNode && n.Tag == "!!bool" && n.Value == s {
		return false
	}
	n.Kind, n.Tag, n.Value, n.Content = yaml.ScalarNode, "!!bool", s, nil
	return true
}

// find walks path through nested mapping nodes starting at m. When create
// is true, missing keys (and non-mapping nodes that need to become mapping
// nodes to hold the next path segment) are created/converted in place.
// Returns nil if not found and create is false.
func (d *Document) find(m *yaml.Node, path []string, create bool) *yaml.Node {
	if len(path) == 0 {
		return m
	}
	if m.Kind != yaml.MappingNode {
		if !create {
			return nil
		}
		m.Kind, m.Tag, m.Content = yaml.MappingNode, "!!map", nil
	}
	key := path[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return d.find(m.Content[i+1], path[1:], create)
		}
	}
	if !create {
		return nil
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := newMapping()
	m.Content = append(m.Content, keyNode, valNode)
	return d.find(valNode, path[1:], create)
}

// Bytes serializes the document back to YAML.
func (d *Document) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d.doc); err != nil {
		return nil, fmt.Errorf("yamlpatch: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yamlpatch: encode: %w", err)
	}
	return buf.Bytes(), nil
}
