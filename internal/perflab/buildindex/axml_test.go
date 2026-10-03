package buildindex

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// metaData is one <meta-data android:name=... android:value=...> element;
// value is a bool or a string.
type metaData struct {
	name  string
	value any
}

// encodeAXML writes a minimal binary manifest the way aapt2 lays it out:
// a UTF-16 string pool, the resource map naming android:name/value, and
// one start-element chunk per meta-data. (The real FixIt manifest carries
// API-key meta-data, so it is not a committed fixture.)
func encodeAXML(t *testing.T, items []metaData) []byte {
	t.Helper()
	le := binary.LittleEndian
	strs := []string{"name", "value", "meta-data", "http://schemas.android.com/apk/res/android"}
	index := func(s string) uint32 {
		for i, x := range strs {
			if x == s {
				return uint32(i)
			}
		}
		strs = append(strs, s)
		return uint32(len(strs) - 1)
	}
	for _, m := range items {
		index(m.name)
		if s, ok := m.value.(string); ok {
			index(s)
		}
	}
	var pool bytes.Buffer
	offsets := make([]uint32, len(strs))
	for i, s := range strs {
		offsets[i] = uint32(pool.Len())
		u := utf16.Encode([]rune(s))
		_ = binary.Write(&pool, le, uint16(len(u)))
		_ = binary.Write(&pool, le, u)
		_ = binary.Write(&pool, le, uint16(0))
	}
	for pool.Len()%4 != 0 {
		pool.WriteByte(0)
	}
	var sp bytes.Buffer
	start := 28 + 4*len(strs)
	put(&sp, uint16(axmlStringPool), uint16(28), uint32(start+pool.Len()), uint32(len(strs)), uint32(0), uint32(0), uint32(start), uint32(0), offsets)
	sp.Write(pool.Bytes())

	var body bytes.Buffer
	body.Write(sp.Bytes())
	put(&body, uint16(axmlResourceMap), uint16(8), uint32(16), uint32(attrName), uint32(attrValue))
	ns := index("http://schemas.android.com/apk/res/android")
	for _, m := range items {
		var attrs bytes.Buffer
		put(&attrs, ns, uint32(0), index(m.name), uint16(8), uint8(0), uint8(typeString), index(m.name))
		switch v := m.value.(type) {
		case bool:
			data := uint32(0)
			if v {
				data = 0xffffffff
			}
			put(&attrs, ns, uint32(1), uint32(0xffffffff), uint16(8), uint8(0), uint8(typeIntBoolean), data)
		case string:
			put(&attrs, ns, uint32(1), index(v), uint16(8), uint8(0), uint8(typeString), index(v))
		}
		put(&body, uint16(axmlStartElement), uint16(16), uint32(16+20+attrs.Len()), uint32(1), uint32(0xffffffff),
			uint32(0xffffffff), index("meta-data"), uint16(20), uint16(20), uint16(2), uint16(0), uint16(0), uint16(0))
		body.Write(attrs.Bytes())
	}
	var out bytes.Buffer
	put(&out, uint16(axmlFile), uint16(8), uint32(8+body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// expoUpdatesValue reads back the ENABLED meta-data's boolean data.
func expoUpdatesValue(t *testing.T, axml []byte) uint32 {
	t.Helper()
	i := bytes.Index(axml, []byte{8, 0, 0, typeIntBoolean})
	if i < 0 {
		t.Fatal("no boolean typed value in the manifest")
	}
	return binary.LittleEndian.Uint32(axml[i+4:])
}

func TestDisableExpoUpdatesFlipsOnlyTheFlag(t *testing.T) {
	in := encodeAXML(t, []metaData{
		{name: "expo.modules.updates.EXPO_UPDATES_CHECK_ON_LAUNCH", value: "ALWAYS"},
		{name: expoUpdatesEnabledKey, value: true},
	})
	out, found, err := disableExpoUpdates(in)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if expoUpdatesValue(t, in) != 0xffffffff || expoUpdatesValue(t, out) != 0 {
		t.Fatal("the ENABLED flag was not flipped to false")
	}
	diff := 0
	for i := range in {
		if in[i] != out[i] {
			diff++
		}
	}
	if len(out) != len(in) || diff != 4 {
		t.Fatalf("the edit must rewrite exactly the 4 data bytes, changed %d (len %d vs %d)", diff, len(out), len(in))
	}

	plain := encodeAXML(t, []metaData{{name: "com.google.android.geo.API_KEY", value: "redacted"}, {name: "other.flag", value: true}})
	out, found, err = disableExpoUpdates(plain)
	if err != nil || found || !bytes.Equal(out, plain) {
		t.Fatalf("an app without expo-updates is left alone: found=%v err=%v", found, err)
	}
	if _, _, err := disableExpoUpdates([]byte("axml")); err == nil {
		t.Fatal("a text manifest must be refused")
	}
}

// put appends little-endian fixed-size values.
func put(b *bytes.Buffer, vs ...any) {
	for _, v := range vs {
		_ = binary.Write(b, binary.LittleEndian, v)
	}
}
