package rerank

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
)

// safetensors is a reader for the HuggingFace safetensors format: an 8-byte
// little-endian header length, a JSON header naming each tensor's dtype, shape
// and byte range, then the raw data. Tensors are read one at a time with
// ReadAt, so loading needs the model's size in memory plus one tensor rather
// than the whole file twice.
type safetensors struct {
	f       *os.File
	base    int64 // offset of the data section
	size    int64
	tensors map[string]tensorInfo
}

type tensorInfo struct {
	DType   string  `json:"dtype"`
	Shape   []int   `json:"shape"`
	Offsets []int64 `json:"data_offsets"`
}

// maxHeader bounds the JSON header, so a corrupt length cannot ask for an
// arbitrary allocation.
const maxHeader = 100 << 20

func openSafetensors(path string) (*safetensors, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening weights: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	s, err := parseSafetensors(f, st.Size())
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.f = f
	return s, nil
}

func parseSafetensors(r io.ReaderAt, size int64) (*safetensors, error) {
	var lenBuf [8]byte
	if _, err := r.ReadAt(lenBuf[:], 0); err != nil {
		return nil, fmt.Errorf("safetensors file too short")
	}
	n := binary.LittleEndian.Uint64(lenBuf[:])
	if n > maxHeader || int64(n)+8 > size {
		return nil, fmt.Errorf("safetensors header length %d is out of range", n)
	}
	hdr := make([]byte, n)
	if _, err := r.ReadAt(hdr, 8); err != nil {
		return nil, fmt.Errorf("reading safetensors header: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &raw); err != nil {
		return nil, fmt.Errorf("parsing safetensors header: %w", err)
	}
	s := &safetensors{base: 8 + int64(n), size: size, tensors: map[string]tensorInfo{}}
	for name, msg := range raw {
		if name == "__metadata__" {
			continue
		}
		var ti tensorInfo
		if err := json.Unmarshal(msg, &ti); err != nil {
			return nil, fmt.Errorf("tensor %q: %w", name, err)
		}
		if len(ti.Offsets) != 2 || ti.Offsets[0] < 0 || ti.Offsets[1] < ti.Offsets[0] ||
			s.base+ti.Offsets[1] > size {
			return nil, fmt.Errorf("tensor %q offsets out of range", name)
		}
		s.tensors[name] = ti
	}
	return s, nil
}

func (s *safetensors) Close() error { return s.f.Close() }

func (s *safetensors) has(name string) bool { _, ok := s.tensors[name]; return ok }

// float32s reads a tensor as float32, converting from F16 or BF16, and checks
// it has the expected shape.
func (s *safetensors) float32s(name string, shape ...int) ([]float32, error) {
	ti, ok := s.tensors[name]
	if !ok {
		return nil, fmt.Errorf("weights are missing tensor %q", name)
	}
	if !sameShape(ti.Shape, shape) {
		return nil, fmt.Errorf("tensor %q has shape %v, expected %v", name, ti.Shape, shape)
	}
	count := 1
	for _, d := range shape {
		count *= d
	}
	var width int
	switch ti.DType {
	case "F32":
		width = 4
	case "F16", "BF16":
		width = 2
	default:
		return nil, fmt.Errorf("tensor %q has unsupported dtype %s", name, ti.DType)
	}
	nbytes := ti.Offsets[1] - ti.Offsets[0]
	if nbytes != int64(count*width) {
		return nil, fmt.Errorf("tensor %q is %d bytes, expected %d", name, nbytes, count*width)
	}
	buf := make([]byte, nbytes)
	if _, err := s.f.ReadAt(buf, s.base+ti.Offsets[0]); err != nil {
		return nil, fmt.Errorf("reading tensor %q: %w", name, err)
	}
	out := make([]float32, count)
	switch ti.DType {
	case "F32":
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case "F16":
		for i := range out {
			out[i] = halfToFloat(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case "BF16":
		for i := range out {
			out[i] = math.Float32frombits(uint32(binary.LittleEndian.Uint16(buf[i*2:])) << 16)
		}
	}
	return out, nil
}

func sameShape(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// halfToFloat widens an IEEE 754 binary16 value exactly, including
// subnormals, infinities and NaN.
func halfToFloat(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h) & 0x3ff
	switch {
	case exp == 0x1f: // inf / NaN
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	case exp == 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal: renormalize into a float32 normal
		e := uint32(127 - 15 + 1)
		for mant&0x400 == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x3ff
		return math.Float32frombits(sign | e<<23 | mant<<13)
	default:
		return math.Float32frombits(sign | (exp+127-15)<<23 | mant<<13)
	}
}
