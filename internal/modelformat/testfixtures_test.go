package modelformat

import (
	"bytes"
	"encoding/binary"
)

// Synthetic fixtures. Building the framing by hand is the point: a fixture
// produced by the same parser under test would prove nothing.

// buildSafetensors returns a file body with the given JSON header text and
// tensor bytes, plus the offset at which the tensor bytes begin.
func buildSafetensors(headerJSON string, tensorBytes []byte) ([]byte, int64) {
	var b bytes.Buffer
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(headerJSON)))
	b.Write(n[:])
	b.WriteString(headerJSON)
	offset := int64(8 + len(headerJSON))
	b.Write(tensorBytes)
	return b.Bytes(), offset
}

type ggufBuilder struct {
	kv      bytes.Buffer
	kvCount uint64

	tensors      bytes.Buffer
	tensorCount  uint64
	alignment    int64
	versionField uint32
}

func newGGUF() *ggufBuilder {
	return &ggufBuilder{alignment: ggufDefaultAlign, versionField: 3}
}

func putU32(b *bytes.Buffer, v uint32) {
	var x [4]byte
	binary.LittleEndian.PutUint32(x[:], v)
	b.Write(x[:])
}

func putU64(b *bytes.Buffer, v uint64) {
	var x [8]byte
	binary.LittleEndian.PutUint64(x[:], v)
	b.Write(x[:])
}

func putStr(b *bytes.Buffer, s string) {
	putU64(b, uint64(len(s)))
	b.WriteString(s)
}

func (g *ggufBuilder) kvString(key, val string) *ggufBuilder {
	putStr(&g.kv, key)
	putU32(&g.kv, ggufString)
	putStr(&g.kv, val)
	g.kvCount++
	return g
}

func (g *ggufBuilder) kvUint32(key string, val uint32) *ggufBuilder {
	putStr(&g.kv, key)
	putU32(&g.kv, ggufUint32)
	putU32(&g.kv, val)
	g.kvCount++
	return g
}

func (g *ggufBuilder) kvFloat64(key string, raw uint64) *ggufBuilder {
	putStr(&g.kv, key)
	putU32(&g.kv, ggufFloat64)
	putU64(&g.kv, raw)
	g.kvCount++
	return g
}

func (g *ggufBuilder) kvInt32Array(key string, vals []int32) *ggufBuilder {
	putStr(&g.kv, key)
	putU32(&g.kv, ggufArray)
	putU32(&g.kv, ggufInt32)
	putU64(&g.kv, uint64(len(vals)))
	for _, v := range vals {
		putU32(&g.kv, uint32(v))
	}
	g.kvCount++
	return g
}

// kvStringArray is the tokenizer-vocabulary shape: a long run of length-prefixed
// strings that the parser has to step over one at a time.
func (g *ggufBuilder) kvStringArray(key string, vals []string) *ggufBuilder {
	putStr(&g.kv, key)
	putU32(&g.kv, ggufArray)
	putU32(&g.kv, ggufString)
	putU64(&g.kv, uint64(len(vals)))
	for _, v := range vals {
		putStr(&g.kv, v)
	}
	g.kvCount++
	return g
}

func (g *ggufBuilder) setAlignment(a uint32) *ggufBuilder {
	g.kvUint32("general.alignment", a)
	g.alignment = int64(a)
	return g
}

func (g *ggufBuilder) tensor(name string, dims []uint64, ggmlType uint32, offset uint64) *ggufBuilder {
	putStr(&g.tensors, name)
	putU32(&g.tensors, uint32(len(dims)))
	for _, d := range dims {
		putU64(&g.tensors, d)
	}
	putU32(&g.tensors, ggmlType)
	putU64(&g.tensors, offset)
	g.tensorCount++
	return g
}

func (g *ggufBuilder) version(v uint32) *ggufBuilder {
	g.versionField = v
	return g
}

// build returns the file body and the offset at which tensor data begins.
func (g *ggufBuilder) build(tensorData []byte) ([]byte, int64) {
	var b bytes.Buffer
	putU32(&b, ggufMagic)
	putU32(&b, g.versionField)
	putU64(&b, g.tensorCount)
	putU64(&b, g.kvCount)
	b.Write(g.kv.Bytes())
	b.Write(g.tensors.Bytes())

	dataOffset := align(int64(b.Len()), g.alignment)
	b.Write(make([]byte, dataOffset-int64(b.Len())))
	b.Write(tensorData)
	return b.Bytes(), dataOffset
}
