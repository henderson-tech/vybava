package findsession

import (
	"bytes"
	"strings"
)

// commonBytes ranks the bytes of Claude transcripts from most to least
// frequent (JSON punctuation, English letters, digits); any byte not listed
// is rarer than all of them.
const commonBytes = " \"etaoinsrhldcu:,pmfygb.wvk{}-_/\\0123456789TSACIEPRNMDLOBFHWGx'()"

// needle is a literal searched by its rarest byte: IndexByte skips through
// the data at vector speed and only that byte's few hits are compared,
// where bytes.Index stalls on every occurrence of a common first byte.
type needle struct {
	text   []byte
	anchor int
}

func newNeedle(text string) needle {
	n := needle{text: []byte(text)}
	best := -1
	for i := 0; i < len(n.text); i++ {
		rank := strings.IndexByte(commonBytes, n.text[i])
		if rank < 0 {
			rank = len(commonBytes)
		}
		if rank > best {
			best, n.anchor = rank, i
		}
	}
	return n
}

func (n needle) in(data []byte) bool {
	a, c := n.anchor, n.text[n.anchor]
	for pos := a; pos < len(data); {
		i := bytes.IndexByte(data[pos:], c)
		if i < 0 {
			return false
		}
		start := pos + i - a
		if start+len(n.text) <= len(data) && bytes.Equal(data[start:start+len(n.text)], n.text) {
			return true
		}
		pos += i + 1
	}
	return false
}
