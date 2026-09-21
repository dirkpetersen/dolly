package model

import "strings"

// IA5Attributes are target attributes with IA5String (ASCII-only) syntax in
// nis.schema. OpenLDAP rejects non-ASCII values in them.
var IA5Attributes = []string{"gecos", "homeDirectory", "loginShell"}

// IsIA5 reports whether attr is ASCII-only on the target.
func IsIA5(attr string) bool {
	for _, a := range IA5Attributes {
		if strings.EqualFold(a, attr) {
			return true
		}
	}
	return false
}

// ToASCII transliterates s to ASCII: accented Latin letters lose their marks
// (é→e, ß→ss, Ø→O), typographic quotes and dashes become ASCII ones, and
// anything else outside ASCII becomes '?'. Control characters are dropped.
func ToASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// drop control characters
		default:
			if t, ok := asciiTable[r]; ok {
				b.WriteString(t)
			} else {
				b.WriteByte('?')
			}
		}
	}
	return b.String()
}
