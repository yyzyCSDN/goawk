// Optional CSV source (provenance) audit support.
//
// When enabled (via Config.SourceAudit), every data value flowing through
// the interpreter carries the set of input data cells it was actually
// computed from. Input cells are identified by input reader instance,
// file name, logical data record number (header rows excluded; quoted
// fields spanning physical lines count as one record), and 1-based
// column number. These counters are independent of the script-mutable
// NR, FNR, and FILENAME variables.
//
// Provenance sets form an immutable DAG: a leaf references one input
// cell, an internal node unions two sets. Merging is therefore O(1),
// and de-duplication happens only when a side-file entry is emitted.

package interp

import (
	"encoding/json"
	"sort"
)

// provRef identifies a single input data cell.
type provRef struct {
	reader int    // input reader instance id (1-based, assigned per opened input)
	file   string // file name as opened ("-" for stdin), independent of FILENAME
	record int    // logical data record number within the reader (1-based, header excluded)
	column int    // 1-based column number within the logical data record
}

// provNode is an immutable provenance set node. A leaf holds a single
// provRef; any other node is the union of sets a and b.
type provNode struct {
	ref  provRef
	a, b *provNode
	leaf bool
}

// auditSource is the machine-readable representation of one input cell,
// emitted as part of a side-file entry.
type auditSource struct {
	Reader int    `json:"reader"`
	File   string `json:"file"`
	Record int    `json:"record"`
	Column int    `json:"column"`
}

// auditEntry is one JSON object per output field, written to the audit
// side file. Sources are de-duplicated and sorted deterministically.
type auditEntry struct {
	Record  int           `json:"record"`
	Field   int           `json:"field"`
	Sources []auditSource `json:"sources"`
}

// provLeaf returns the interned leaf node for the given cell reference.
func (p *interp) provLeaf(ref provRef) *provNode {
	if node, ok := p.provLeaves[ref]; ok {
		return node
	}
	node := &provNode{ref: ref, leaf: true}
	p.provLeaves[ref] = node
	return node
}

// provMerge returns the union of the two provenance sets. A nil set
// stands for the empty set (constants, unaudited runs).
func (p *interp) provMerge(a, b *provNode) *provNode {
	if a == nil {
		return b
	}
	if b == nil || a == b {
		return a
	}
	return &provNode{a: a, b: b}
}

// provMergeAll unions the provenance sets of the given nodes.
func (p *interp) provMergeAll(nodes []*provNode) *provNode {
	var result *provNode
	for _, node := range nodes {
		result = p.provMerge(result, node)
	}
	return result
}

// provMergeValues unions the provenance sets of the given values.
func (p *interp) provMergeValues(values ...value) *provNode {
	var result *provNode
	for _, v := range values {
		result = p.provMerge(result, v.prov)
	}
	return result
}

// withProv returns v with its provenance set replaced by prov.
func withProv(v value, prov *provNode) value {
	v.prov = prov
	return v
}

// provCollect walks the provenance DAG and returns its de-duplicated,
// sorted cell references. A nil root yields an empty (non-nil) slice.
func (p *interp) provCollect(root *provNode) []auditSource {
	seen := make(map[provRef]struct{})
	if root != nil {
		stack := []*provNode{root}
		for len(stack) > 0 {
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if node.leaf {
				seen[node.ref] = struct{}{}
				continue
			}
			if node.a != nil {
				stack = append(stack, node.a)
			}
			if node.b != nil {
				stack = append(stack, node.b)
			}
		}
	}

	refs := make([]provRef, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		a, b := refs[i], refs[j]
		if a.reader != b.reader {
			return a.reader < b.reader
		}
		if a.record != b.record {
			return a.record < b.record
		}
		if a.column != b.column {
			return a.column < b.column
		}
		return a.file < b.file
	})

	sources := make([]auditSource, len(refs))
	for i, ref := range refs {
		sources[i] = auditSource{
			Reader: ref.reader,
			File:   ref.file,
			Record: ref.record,
			Column: ref.column,
		}
	}
	return sources
}

// auditEmit writes one side-file entry per output field of a "print"
// with arguments. The normal output bytes are written separately and
// are never affected by this.
func (p *interp) auditEmit(args []value) error {
	p.auditRecordNum++
	encoder := json.NewEncoder(p.auditWriter)
	for i, arg := range args {
		entry := auditEntry{
			Record:  p.auditRecordNum,
			Field:   i + 1,
			Sources: p.provCollect(arg.prov),
		}
		if err := encoder.Encode(entry); err != nil {
			return newError("error writing source audit file: %s", err)
		}
	}
	return nil
}

// errAuditUnsupported builds the error used when the script performs an
// operation whose value sources the audit can't track. Rather than
// emitting an incomplete provenance set, execution is refused.
func errAuditUnsupported(what string) error {
	return newError("source audit: %s is not supported; provenance can't be tracked for it", what)
}

// keyProvMap returns the per-key provenance map for the given array slot.
func (p *interp) keyProvMap(arraySlot int) map[string]*provNode {
	m := p.arrayKeyProv[arraySlot]
	if m == nil {
		m = make(map[string]*provNode)
		p.arrayKeyProv[arraySlot] = m
	}
	return m
}

// arrayReadProv reads an array element. The element's sources are merged
// with the sources of the current index expression. A non-existent
// element is autovivified (per POSIX), and the index becomes a key
// source.
func (p *interp) arrayReadProv(arraySlot int, indexVal value) value {
	array := p.arrays[arraySlot]
	key := p.toString(indexVal)
	v, ok := array[key]
	if !ok {
		array[key] = v
		if p.audit {
			m := p.keyProvMap(arraySlot)
			m[key] = p.provMerge(m[key], indexVal.prov)
		}
	}
	if p.audit {
		v.prov = p.provMerge(v.prov, indexVal.prov)
	}
	return v
}

// arrayWriteProv writes an element value. The index sources are not
// mixed into the element value, but are unioned into the key's sources.
func (p *interp) arrayWriteProv(arraySlot int, indexVal, v value) {
	key := p.toString(indexVal)
	p.arrays[arraySlot][key] = v
	if p.audit {
		m := p.keyProvMap(arraySlot)
		m[key] = p.provMerge(m[key], indexVal.prov)
	}
}

// arrayDeleteProv deletes an element together with its key sources.
func (p *interp) arrayDeleteProv(arraySlot int, key string) {
	delete(p.arrays[arraySlot], key)
	if p.audit {
		delete(p.keyProvMap(arraySlot), key)
	}
}

// arrayDeleteAllProv removes all elements and all key sources.
func (p *interp) arrayDeleteAllProv(arraySlot int) {
	for k := range p.arrays[arraySlot] {
		delete(p.arrays[arraySlot], k)
	}
	if p.audit {
		m := p.arrayKeyProv[arraySlot]
		for k := range m {
			delete(m, k)
		}
	}
}

// arrayKeyProvUnion merges the key sources of all surviving keys.
func (p *interp) arrayKeyProvUnion(arraySlot int) *provNode {
	if !p.audit {
		return nil
	}
	var result *provNode
	for _, node := range p.arrayKeyProv[arraySlot] {
		result = p.provMerge(result, node)
	}
	return result
}
