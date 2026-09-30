package framestats

import "fmt"

// A schema-less protobuf wire reader: just enough to walk a Perfetto trace
// without the generated protos (and their dependency tree).

// field is one decoded protobuf field: varint/fixed values in u, length
// delimited payloads in data.
type field struct {
	num  int
	wt   int
	u    uint64
	data []byte
}

func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i >= 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, -1
}

// fields decodes one message level. A malformed or truncated tail returns
// the fields decoded so far AND an error naming the byte offset: a partial
// message is never silently taken for a whole one.
func fields(b []byte) ([]field, error) {
	var out []field
	size := len(b)
	for len(b) > 0 {
		at := size - len(b)
		key, n := uvarint(b)
		if n <= 0 {
			return out, fmt.Errorf("bad field key at byte %d of %d", at, size)
		}
		b = b[n:]
		f := field{num: int(key >> 3), wt: int(key & 7)}
		switch f.wt {
		case 0:
			v, n := uvarint(b)
			if n <= 0 {
				return out, fmt.Errorf("field %d: truncated varint at byte %d of %d", f.num, at, size)
			}
			f.u = v
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return out, fmt.Errorf("field %d: truncated fixed64 at byte %d of %d", f.num, at, size)
			}
			for i := 0; i < 8; i++ {
				f.u |= uint64(b[i]) << (8 * i)
			}
			b = b[8:]
		case 2:
			l, n := uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return out, fmt.Errorf("field %d: length runs past the end at byte %d of %d", f.num, at, size)
			}
			f.data = b[n : n+int(l)]
			b = b[n+int(l):]
		case 5:
			if len(b) < 4 {
				return out, fmt.Errorf("field %d: truncated fixed32 at byte %d of %d", f.num, at, size)
			}
			for i := 0; i < 4; i++ {
				f.u |= uint64(b[i]) << (8 * i)
			}
			b = b[4:]
		default:
			return out, fmt.Errorf("field %d: unsupported wire type %d at byte %d of %d", f.num, f.wt, at, size)
		}
		out = append(out, f)
	}
	return out, nil
}
