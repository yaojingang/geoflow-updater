package tuf

import _ "embed"

// TrustedRoot is the offline-signed trust anchor embedded in every updater build.
//
//go:embed repository/metadata/root.json
var TrustedRoot []byte
