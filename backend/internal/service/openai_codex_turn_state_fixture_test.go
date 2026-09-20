package service

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"time"
)

var codexFixtureIssued = time.Now().UTC().Add(-time.Minute).Truncate(time.Second)

func codexTestState(fill string, length int) string {
	return codexTestStateAt(fill, length, codexFixtureIssued)
}
func codexTestStateAt(fill string, length int, issued time.Time) string {
	size := 217
	if length == 332 {
		size = 249
	}
	raw := bytes.Repeat([]byte(fill), size)
	raw = raw[:size]
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}
