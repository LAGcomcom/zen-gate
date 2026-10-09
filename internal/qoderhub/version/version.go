package version

// Version is the embedded QoderCN gateway build version. It is injected at build time via
//
//	-ldflags "-X zen-gate/internal/qoderhub/version.Version=$(cat VERSION)"
//
// (see the Dockerfile and .github/workflows/release.yml). A plain `go build` /
// `go run` without that flag reports "dev". The VERSION file is the single source.
var Version = "dev"
