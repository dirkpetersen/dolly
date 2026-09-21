package model

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// FormatGUID converts AD's 16-byte objectGUID to the canonical lowercase
// hyphenated form. AD stores the first three fields little-endian
// (mixed-endian), so bytes 0-3, 4-5, and 6-7 are reversed.
func FormatGUID(b []byte) (string, error) {
	if len(b) != 16 {
		return "", fmt.Errorf("objectGUID must be 16 bytes, got %d", len(b))
	}
	o := []byte{b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6]}
	o = append(o, b[8:]...)
	h := hex.EncodeToString(o)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// ParseGUID validates a GUID string (optionally in braces, any case) and
// returns it in canonical lowercase hyphenated form.
func ParseGUID(s string) (string, error) {
	t := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "{"), "}"))
	if len(t) != 36 || t[8] != '-' || t[13] != '-' || t[18] != '-' || t[23] != '-' {
		return "", fmt.Errorf("invalid GUID %q", s)
	}
	if _, err := hex.DecodeString(strings.ReplaceAll(t, "-", "")); err != nil {
		return "", fmt.Errorf("invalid GUID %q", s)
	}
	return t, nil
}
