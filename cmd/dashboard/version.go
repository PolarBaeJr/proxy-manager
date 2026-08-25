package main

// buildVersion is a monotonic integer build number (GitHub Actions'
// github.run_number), injected at image-build time via
// `-ldflags -X main.buildVersion=...` (see Dockerfile / build.yml).
// A plain `go build` with no ldflags leaves it at the literal "dev" —
// every consumer must check for "dev" explicitly before any numeric
// parse/compare; never treat it as version 0.
var buildVersion = "dev"
