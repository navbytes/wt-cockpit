package buildinfo

import "testing"

func TestVersionPrefersLdflags(t *testing.T) {
	if got := Version("v0.3.0"); got != "v0.3.0" {
		t.Fatalf("ldflags version should win, got %q", got)
	}
}

func TestVersionFallsBackFromDev(t *testing.T) {
	// In `go test`, ReadBuildInfo().Main.Version is "" or "(devel)", so a "dev"
	// ldflags value has nothing better to fall back to and stays "dev". This
	// pins that the fallback never panics and never invents a version.
	if got := Version("dev"); got != "dev" {
		t.Fatalf("dev with no module version should stay dev, got %q", got)
	}
	if got := Version(""); got != "" {
		t.Fatalf("empty with no module version should stay empty, got %q", got)
	}
}
