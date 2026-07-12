// Package packaging holds no production code — only tests over the static
// service-definition files shipped in this directory (go test's working
// directory for a package is always that package's own source directory, so
// these read the plist/service files by their plain relative names).
package packaging

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// TestPlistLintsClean validates com.wtcockpit.wtd.plist with whichever system
// XML/plist validator is available — plutil (macOS-native, stricter about
// plist semantics) is preferred, falling back to xmllint (well-formedness
// only). Skipped cleanly, per the brief, when neither tool exists (e.g. a
// minimal Linux CI image) rather than failing the run over a missing tool.
func TestPlistLintsClean(t *testing.T) {
	const path = "com.wtcockpit.wtd.plist"
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("plist not found: %v", err)
	}

	switch {
	case lookPath("plutil"):
		if out, err := exec.Command("plutil", "-lint", path).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint %s failed: %v\n%s", path, err, out)
		}
	case lookPath("xmllint"):
		if out, err := exec.Command("xmllint", "--noout", path).CombinedOutput(); err != nil {
			t.Errorf("xmllint --noout %s failed: %v\n%s", path, err, out)
		}
	default:
		t.Skip("neither plutil nor xmllint is available on this machine")
	}
}

// TestServiceFileHasExecStartAndWantedBy sanity-checks wtd.service's two
// load-bearing directives: without ExecStart, systemd has nothing to run;
// without WantedBy, `systemctl enable` has no target to hook into, so the
// unit silently never starts at boot.
func TestServiceFileHasExecStartAndWantedBy(t *testing.T) {
	const path = "wtd.service"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	content := string(b)
	if !strings.Contains(content, "ExecStart=") {
		t.Errorf("%s missing ExecStart=; content:\n%s", path, content)
	}
	if !strings.Contains(content, "WantedBy=") {
		t.Errorf("%s missing WantedBy=; content:\n%s", path, content)
	}
}
