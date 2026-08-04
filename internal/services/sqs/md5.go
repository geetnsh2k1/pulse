package sqs

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

// MD5Hex is used for MD5OfMessageBody; SDKs (botocore in particular) verify
// it against what they sent. Exported for the ESM event builder.
func MD5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func md5Hex(s string) string { return MD5Hex(s) }

// md5OfAttributes implements AWS's documented message-attribute digest:
// attributes sorted by name; for each, length-prefixed name, data type, and
// a transport byte (1 = string value, 2 = binary value) followed by the
// length-prefixed value. All lengths are 4-byte big-endian.
func md5OfAttributes(attrs map[string]MessageAttribute) string {
	if len(attrs) == 0 {
		return ""
	}
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	sort.Strings(names)

	h := md5.New()
	writeChunk := func(b []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(b)))
		h.Write(l[:])
		h.Write(b)
	}
	for _, name := range names {
		a := attrs[name]
		writeChunk([]byte(name))
		writeChunk([]byte(a.DataType))
		if a.BinaryValue != "" {
			h.Write([]byte{2})
			raw, err := base64.StdEncoding.DecodeString(a.BinaryValue)
			if err != nil {
				raw = []byte(a.BinaryValue)
			}
			writeChunk(raw)
		} else {
			h.Write([]byte{1})
			writeChunk([]byte(a.StringValue))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
