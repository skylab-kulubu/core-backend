// Package config carries reviewed configuration files inside the binary, so a
// running core always uses the version that passed review with its code.
package config

import _ "embed"

// MediaPurposes is the Media purpose catalogue (media-purposes.json). Core
// validates it against the hard ceilings in internal/media at startup.
//
//go:embed media-purposes.json
var MediaPurposes []byte
