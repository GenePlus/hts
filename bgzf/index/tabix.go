// Copyright ©2014 The bíogo.bam Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"code.google.com/p/biogo.bam/bgzf"
	"code.google.com/p/biogo.bam/internal"
	"code.google.com/p/biogo.bam/sam"
)

// Tabix is a tabix index.
type Tabix struct {
	Format    byte
	ZeroBased bool

	NameColumn  int32
	BeginColumn int32
	EndColumn   int32

	MetaChar rune
	Skip     int32

	refNames []string
	nameMap  map[string]int

	idx internal.Index
}

// NewTabix returns a new Tabix index.
func NewTabix() *Tabix {
	return &Tabix{nameMap: make(map[string]int)}
}

// NumRefs returns the number of references in the index.
func (i *Tabix) NumRefs() int {
	return len(i.idx.Refs)
}

// Names returns the reference names in the index. The returned
// slice should not be altered.
func (i *Tabix) Names() []string {
	return i.refNames
}

// ReferenceStats returns the index statistics for the given reference and true
// if the statistics are valid.
func (i *Tabix) ReferenceStats(id int) (stats ReferenceStats, ok bool) {
	s := i.idx.Refs[id].Stats
	if s == nil {
		return ReferenceStats{}, false
	}
	return ReferenceStats(*s), true
}

// Unmapped returns the number of unmapped reads and true if the count is valid.
func (i *Tabix) Unmapped() (n uint64, ok bool) {
	if i.idx.Unmapped == nil {
		return 0, false
	}
	return *i.idx.Unmapped, true
}

// TabixRecord wraps types that may be indexed by a Tabix.
type TabixRecord interface {
	RefName() string
	Start() int
	End() int
}

type tabixShim struct {
	id, start, end int
}

func (r tabixShim) RefID() int { return r.id }
func (r tabixShim) Start() int { return r.start }
func (r tabixShim) End() int   { return r.end }

// Add records the SAM record as having being located at the given chunk.
func (i *Tabix) Add(r TabixRecord, c bgzf.Chunk, placed, mapped bool) error {
	refName := r.RefName()
	rid, ok := i.nameMap[refName]
	if !ok {
		rid = len(i.refNames)
		i.refNames = append(i.refNames, refName)
	}
	shim := tabixShim{id: rid, start: r.Start(), end: r.End()}
	return i.idx.Add(shim, internal.BinFor(r.Start(), r.End()), c, placed, mapped)
}

// Chunks returns a []bgzf.Chunk that corresponds to the given genomic interval.
func (i *Tabix) Chunks(r *sam.Reference, beg, end int) []bgzf.Chunk {
	chunks := i.idx.Chunks(r.ID(), beg, end)
	return Adjacent(chunks)
}

// MergeChunks applies the given MergeStrategy to all bins in the Index.
func (i *Tabix) MergeChunks(s MergeStrategy) {
	i.idx.MergeChunks(s)
}

var tbiMagic = [4]byte{'T', 'B', 'I', 0x1}

// ReadTabix reads the tabix index from the given io.Reader.
func ReadTabix(r io.Reader) (*Tabix, error) {
	var (
		idx   Tabix
		magic [4]byte
		err   error
	)
	err = binary.Read(r, binary.LittleEndian, &magic)
	if err != nil {
		return nil, err
	}
	if magic != tbiMagic {
		return nil, errors.New("tabix: magic number mismatch")
	}

	var n int32
	err = binary.Read(r, binary.LittleEndian, &n)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}

	err = readTabixHeader(r, &idx)
	if err != nil {
		return nil, err
	}
	if len(idx.refNames) != int(n) {
		return nil, fmt.Errorf("tabix: name count mismatch: %d != %d", len(idx.refNames), n)
	}
	idx.nameMap = make(map[string]int)
	for i, n := range idx.refNames {
		idx.nameMap[n] = i
	}

	idx.idx, err = internal.ReadIndex(r, n, "tabix")
	if err != nil {
		return nil, err
	}
	return &idx, nil
}

func readTabixHeader(r io.Reader, idx *Tabix) error {
	var (
		format int32
		err    error
	)
	err = binary.Read(r, binary.LittleEndian, &format)
	if err != nil {
		return fmt.Errorf("tabix: failed to read format: %v", err)
	}
	idx.Format = byte(format)
	idx.ZeroBased = format&0x10000 != 0

	err = binary.Read(r, binary.LittleEndian, &idx.NameColumn)
	if err != nil {
		return fmt.Errorf("tabix: failed to read name column index: %v", err)
	}
	err = binary.Read(r, binary.LittleEndian, &idx.BeginColumn)
	if err != nil {
		return fmt.Errorf("tabix: failed to read begin column index: %v", err)
	}
	err = binary.Read(r, binary.LittleEndian, &idx.EndColumn)
	if err != nil {
		return fmt.Errorf("tabix: failed to read end column index: %v", err)
	}
	err = binary.Read(r, binary.LittleEndian, &idx.MetaChar)
	if err != nil {
		return fmt.Errorf("tabix: failed to read metacharacter: %v", err)
	}
	err = binary.Read(r, binary.LittleEndian, &idx.Skip)
	if err != nil {
		return fmt.Errorf("tabix: failed to read skip count: %v", err)
	}
	var n int32
	err = binary.Read(r, binary.LittleEndian, &n)
	if err != nil {
		return fmt.Errorf("tabix: failed to read name lengths: %v", err)
	}
	nameBytes := make([]byte, n)
	_, err = r.Read(nameBytes)
	if err != nil {
		return fmt.Errorf("tabix: failed to read names: %v", err)
	}
	names := string(nameBytes)
	if names[len(names)-1] != 0 {
		return errors.New("tabix: last name not zero-terminated")
	}
	idx.refNames = strings.Split(names[:len(names)-1], string(0))

	return nil
}

// WriteTabix writes the index to the given io.Writer.
func WriteTabix(w io.Writer, idx *Tabix) error {
	err := binary.Write(w, binary.LittleEndian, tbiMagic)
	if err != nil {
		return err
	}

	err = binary.Write(w, binary.LittleEndian, int32(len(idx.idx.Refs)))
	if err != nil {
		return err
	}
	err = writeTabixHeader(w, idx)
	if err != nil {
		return err
	}

	return internal.WriteIndex(w, &idx.idx, "tabix")
}

func writeTabixHeader(w io.Writer, idx *Tabix) error {
	var err error
	format := int32(idx.Format)
	if idx.ZeroBased {
		format |= 0x10000
	}
	err = binary.Write(w, binary.LittleEndian, format)
	if err != nil {
		return fmt.Errorf("tabix: failed to write format: %v", err)
	}
	err = binary.Write(w, binary.LittleEndian, idx.NameColumn)
	if err != nil {
		return fmt.Errorf("tabix: failed to write name column index: %v", err)
	}
	err = binary.Write(w, binary.LittleEndian, idx.BeginColumn)
	if err != nil {
		return fmt.Errorf("tabix: failed to write begin column index: %v", err)
	}
	err = binary.Write(w, binary.LittleEndian, idx.EndColumn)
	if err != nil {
		return fmt.Errorf("tabix: failed to write end column index: %v", err)
	}
	err = binary.Write(w, binary.LittleEndian, idx.MetaChar)
	if err != nil {
		return fmt.Errorf("tabix: failed to write metacharacter: %v", err)
	}
	err = binary.Write(w, binary.LittleEndian, idx.Skip)
	if err != nil {
		return fmt.Errorf("tabix: failed to write skip count: %v", err)
	}
	var n int32
	for _, name := range idx.refNames {
		n += int32(len(name) + 1)
	}
	err = binary.Write(w, binary.LittleEndian, n)
	if err != nil {
		return fmt.Errorf("tabix: failed to write name lengths: %v", err)
	}
	for _, name := range idx.refNames {
		_, err = w.Write([]byte(name))
		if err != nil {
			return fmt.Errorf("tabix: failed to write name: %v", err)
		}
		_, err = w.Write([]byte{0})
		if err != nil {
			return fmt.Errorf("tabix: failed to write name: %v", err)
		}
	}
	return nil
}
