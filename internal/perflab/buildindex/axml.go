package buildindex

import (
	"encoding/binary"
	"errors"
	"unicode/utf16"
)

// The binary AndroidManifest.xml (AXML) edit pack needs: flip the boolean
// <meta-data android:name="expo.modules.updates.ENABLED" android:value=true>
// to false, the Android twin of iOS pack's EXUpdatesEnabled=NO. Without it
// a swapped variant keeps expo-updates' launch check, and a phone with
// network can download an OTA update and run that bundle on the next cold
// start: a silent confound APK-sha fencing cannot see. The edit rewrites 4
// bytes of one typed value in place; the string pool is untouched.

const (
	axmlFile         = 0x0003
	axmlStringPool   = 0x0001
	axmlResourceMap  = 0x0180
	axmlStartElement = 0x0102
	axmlUTF8Flag     = 1 << 8
	typeString       = 0x03
	typeIntBoolean   = 0x12
	attrName         = 0x01010003 // android:name
	attrValue        = 0x01010024 // android:value
)

// expoUpdatesEnabledKey is the meta-data expo-updates reads on Android.
const expoUpdatesEnabledKey = "expo.modules.updates.ENABLED"

var errNotAXML = errors.New("not a binary AndroidManifest.xml")

// disableExpoUpdates returns a copy of the manifest with expo-updates'
// ENABLED meta-data set to false; found is false when the app has no such
// meta-data (no expo-updates, nothing to disable).
func disableExpoUpdates(axml []byte) (out []byte, found bool, err error) {
	le := binary.LittleEndian
	if len(axml) < 8 || le.Uint16(axml) != axmlFile {
		return nil, false, errNotAXML
	}
	out = append([]byte(nil), axml...)
	var strs []string
	var resIDs []uint32
	pos := int(le.Uint16(axml[2:]))
	for pos+8 <= len(out) {
		typ, hdr, size := le.Uint16(out[pos:]), int(le.Uint16(out[pos+2:])), int(le.Uint32(out[pos+4:]))
		if size < 8 || pos+size > len(out) {
			return nil, false, errNotAXML
		}
		chunk := out[pos : pos+size]
		switch typ {
		case axmlStringPool:
			if strs, err = readStringPool(chunk); err != nil {
				return nil, false, err
			}
		case axmlResourceMap:
			for i := hdr; i+4 <= size; i += 4 {
				resIDs = append(resIDs, le.Uint32(chunk[i:]))
			}
		case axmlStartElement:
			if patchMetaData(chunk, hdr, strs, resIDs) {
				found = true
			}
		}
		pos += size
	}
	return out, found, nil
}

// patchMetaData flips the value of the ENABLED meta-data element in place.
func patchMetaData(chunk []byte, hdr int, strs []string, resIDs []uint32) bool {
	le := binary.LittleEndian
	if hdr+20 > len(chunk) || str(strs, le.Uint32(chunk[hdr+4:])) != "meta-data" {
		return false
	}
	start, size, count := int(le.Uint16(chunk[hdr+8:])), int(le.Uint16(chunk[hdr+10:])), int(le.Uint16(chunk[hdr+12:]))
	is := func(nameIdx uint32, local string, id uint32) bool {
		if int(nameIdx) < len(resIDs) && resIDs[nameIdx] == id {
			return true
		}
		return str(strs, nameIdx) == local
	}
	valueAt, named := -1, false
	for i := 0; i < count; i++ {
		a := hdr + start + i*size
		if a+20 > len(chunk) {
			return false
		}
		name := le.Uint32(chunk[a+4:])
		switch {
		case is(name, "name", attrName):
			named = str(strs, le.Uint32(chunk[a+8:])) == expoUpdatesEnabledKey ||
				(chunk[a+15] == typeString && str(strs, le.Uint32(chunk[a+16:])) == expoUpdatesEnabledKey)
		case is(name, "value", attrValue) && chunk[a+15] == typeIntBoolean:
			valueAt = a + 16
		}
	}
	if !named || valueAt < 0 {
		return false
	}
	le.PutUint32(chunk[valueAt:], 0)
	return true
}

func str(strs []string, i uint32) string {
	if int(i) < len(strs) {
		return strs[i]
	}
	return ""
}

// readStringPool decodes a ResStringPool chunk (UTF-8 or UTF-16).
func readStringPool(chunk []byte) ([]string, error) {
	le := binary.LittleEndian
	if len(chunk) < 28 {
		return nil, errNotAXML
	}
	hdr := int(le.Uint16(chunk[2:]))
	count, flags, start := int(le.Uint32(chunk[8:])), le.Uint32(chunk[16:]), int(le.Uint32(chunk[20:]))
	if hdr+4*count > len(chunk) || start > len(chunk) {
		return nil, errNotAXML
	}
	out := make([]string, count)
	for i := range out {
		p := start + int(le.Uint32(chunk[hdr+4*i:]))
		if p >= len(chunk) {
			return nil, errNotAXML
		}
		if flags&axmlUTF8Flag != 0 {
			_, n := utf8Len(chunk, p) // utf-16 length, unused
			n8, m := utf8Len(chunk, p+n)
			b := p + n + m
			if b+n8 > len(chunk) {
				return nil, errNotAXML
			}
			out[i] = string(chunk[b : b+n8])
			continue
		}
		n16, m := utf16Len(chunk, p)
		b := p + m
		if b+2*n16 > len(chunk) {
			return nil, errNotAXML
		}
		u := make([]uint16, n16)
		for j := range u {
			u[j] = le.Uint16(chunk[b+2*j:])
		}
		out[i] = string(utf16.Decode(u))
	}
	return out, nil
}

// utf8Len reads a UTF-8 pool length (1 or 2 bytes) at p.
func utf8Len(b []byte, p int) (n, width int) {
	if p >= len(b) {
		return 0, 1
	}
	if b[p]&0x80 == 0 {
		return int(b[p]), 1
	}
	if p+1 >= len(b) {
		return 0, 2
	}
	return int(b[p]&0x7f)<<8 | int(b[p+1]), 2
}

// utf16Len reads a UTF-16 pool length (1 or 2 units) at p.
func utf16Len(b []byte, p int) (n, width int) {
	if p+2 > len(b) {
		return 0, 2
	}
	v := int(binary.LittleEndian.Uint16(b[p:]))
	if v&0x8000 == 0 {
		return v, 2
	}
	if p+4 > len(b) {
		return 0, 4
	}
	return (v&0x7fff)<<16 | int(binary.LittleEndian.Uint16(b[p+2:])), 4
}
