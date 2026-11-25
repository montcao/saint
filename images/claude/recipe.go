// Package claude supplies the trusted recipe shipped with the Saint binary.
package claude

import _ "embed"

//go:embed Dockerfile
var dockerfile string

// Recipe returns a fresh copy; projects and caller working directories cannot
// override the image recipe used by session creation, setup or doctor.
func Recipe() []byte { return []byte(dockerfile) }
