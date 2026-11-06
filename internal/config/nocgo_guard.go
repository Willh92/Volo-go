//go:build cgo && !race

package config

// Compile-time enforcement of Volo-go's strict CGO_ENABLED=0 invariant.
// Any attempt to build with CGO_ENABLED=1 fails immediately at compile time.
var _ = CGO_IS_STRICTLY_FORBIDDEN_BUILD_WITH_CGO_ENABLED_0
