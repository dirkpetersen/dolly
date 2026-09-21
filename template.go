// Package dolly holds files embedded into the binary from the repository
// root. The code lives under cmd/ and internal/.
package dolly

import _ "embed"

// ConfigTemplate is dolly.yaml.template, used by `dolly install` to create a
// config and by tests to check that the template stays valid.
//
//go:embed dolly.yaml.template
var ConfigTemplate []byte
