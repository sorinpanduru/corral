package mongofx

// Images are pinned by digest (ADR-0022). The digests are the multi-platform
// index digests of the official images mongo:7.0 and mongo:8.0 as of
// 2026-10-24. Update them deliberately, together with a full functional run.
var images = map[string]string{
	"7.0": "docker.io/library/mongo@sha256:c53852c00c23ca9f23fc39690350ffa9a80e3642fe8f96879726d8da0abe80a7",
	"8.0": "docker.io/library/mongo@sha256:4a4ab5d4834207389e0d7270fac988e21ffedccecba77991a552f755df229907",
}

// Versions returns the supported MongoDB versions, oldest first.
func Versions() []string { return []string{"7.0", "8.0"} }

// Image returns the digest-pinned image for version, or "" if unknown.
func Image(version string) string { return images[version] }
