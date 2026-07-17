// Package assets bundles build-time data files (DESIGN §9). The pricing
// registry is versioned, user-overridable, and refreshed only on explicit
// `relay pricing update` — never automatically (that would be phone-home).
package assets

import _ "embed"

// PricingJSON is the bundled pricing registry.
//
//go:embed pricing.json
var PricingJSON []byte
