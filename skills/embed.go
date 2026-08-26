// Package skills contains the hand-authored source for delegate's managed skill.
package skills

import _ "embed"

// Delegate is installed for each supported host.
//
//go:embed delegate/SKILL.md
var Delegate string
