package web

// P5 WP3: the reading-room guardrail banner's severity tinting + control-byte
// defense, and the guardrail.tripped web toast (P5-design.md WP3; the
// orchestrating brief's §1.5). buildRepoEngine(WithRules)/mustWriteFile/
// mustResolver/stubAPI/getPage/testBoundAddr/buildTestApp come from
// room_test.go/web_test.go, same package.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/guardrail"
)

// ---- banner severity tinting ----

// TestRoomHandlerBannerTintedDangerForDangerHit extends
// TestRoomHandlerRendersGuardrailBannerWhenTripped (room_test.go): a
// danger-severity featured hit must tint the banner itself danger-red, not
// just the file's own tag.
func TestRoomHandlerBannerTintedDangerForDangerHit(t *testing.T) {
	eng, feat := buildRepoEngine(t, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "migrations", "014_drop.sql"), []byte("DROP TABLE x;\n"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	body := getPage(t, h, "/wt/"+feat.ID).Body.String()
	if !strings.Contains(body, `class="alert danger"`) {
		t.Errorf("expected the banner tinted danger for a danger-severity featured hit, got:\n%s", body)
	}
}

// TestRoomHandlerBannerNotTintedDangerForWarnOnlyHit is the other half: a
// worktree tripping only a warn-severity rule must keep the plain (amber)
// alert styling, not the new danger variant.
func TestRoomHandlerBannerNotTintedDangerForWarnOnlyHit(t *testing.T) {
	rules := []guardrail.Rule{{Name: "warn-only", Severity: "warn", PathGlob: "**/*.go", Message: "touched a go file"}}
	eng, feat := buildRepoEngineWithRules(t, rules, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "extra.go"), []byte("package extra\n"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	body := getPage(t, h, "/wt/"+feat.ID).Body.String()
	if !strings.Contains(body, `<b>Guardrail:</b> touched a go file`) {
		t.Fatalf("expected the warn hit's message in the banner, got:\n%s", body)
	}
	if strings.Contains(body, `class="alert danger"`) {
		t.Errorf("a warn-only hit must not tint the banner danger, got:\n%s", body)
	}
	if !strings.Contains(body, `class="alert"`) {
		t.Errorf("expected the plain (non-danger) alert class, got:\n%s", body)
	}
}

// ---- guardrail Message: escaping + control-byte torture ----

// TestRoomHandlerGuardrailMessageEscapedAndControlBytesCaretified is the
// phase's explicit torture case: a rule's Message is hand-authored (a repo's
// own .wtcockpit.toml pack is semi-trusted input, P5-design.md §1.3) and
// never passes through diffparse's own sanitizing pass the way diff content/
// paths already do. `<script>` must render html-escaped (html/template's
// ordinary contextual escaping); a raw control byte must render as caret
// notation (diffparse.SanitizeControl, reused — see guardrailBanner's own
// doc comment).
func TestRoomHandlerGuardrailMessageEscapedAndControlBytesCaretified(t *testing.T) {
	rules := []guardrail.Rule{{
		Name:     "hostile-message",
		Severity: "danger",
		PathGlob: "**/*.go",
		Message:  "<script>alert(1)</script> \x1b[31mred\x1b[0m",
	}}
	eng, feat := buildRepoEngineWithRules(t, rules, func(_, wt string) {
		mustWriteFile(t, filepath.Join(wt, "extra.go"), []byte("package extra\n"))
	})
	h := New(eng, stubAPI(), Config{BoundAddr: testBoundAddr, CSRFToken: "tok"})

	body := getPage(t, h, "/wt/"+feat.ID).Body.String()

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("expected the hostile rule message's <script> escaped, got:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("expected the hostile rule message html-escaped, got:\n%s", body)
	}
	if strings.ContainsAny(body, "\x1b") {
		t.Error("raw ESC control byte leaked into the response body from a pack-authored guardrail Message")
	}
	if !strings.Contains(body, "^[") {
		t.Error(`expected the control byte caret-sanitized to "^["`)
	}
}

// ---- toast: static container + app.js DOM-safety ----

// TestPagesContainToastRegionContainer pins that every page (index and room
// alike — the toast is shared connectEvents plumbing, not room-scoped)
// renders exactly one static, empty mount point for app.js's toasts.
func TestPagesContainToastRegionContainer(t *testing.T) {
	h, feat, _ := buildTestApp(t)
	for _, path := range []string{"/", "/wt/" + feat.ID} {
		body := getPage(t, h, path).Body.String()
		if n := strings.Count(body, `id="toast-region"`); n != 1 {
			t.Errorf("%s: toast-region container appears %d times, want exactly 1, got:\n%s", path, n, body)
		}
	}
}

// TestAppJSGuardrailToastBuildsDOMWithoutInnerHTML is the toast's own
// boundary-rule proof, alongside room_test.go's whole-file
// TestAppJSKeepsBoundaryRulesInForce (which already pins the file's total
// innerHTML-assignment count at exactly 1 — showGuardrailToast adds none, so
// that test alone would already catch a regression here). This one names the
// specific function so a future break is unambiguous about which code
// violated the rule, and separately confirms the SSE handler actually gates
// on severity before calling it.
func TestAppJSGuardrailToastBuildsDOMWithoutInnerHTML(t *testing.T) {
	src, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)

	start := strings.Index(js, "function showGuardrailToast")
	if start < 0 {
		t.Fatal("expected a showGuardrailToast function in app.js")
	}
	fn := js[start:]
	if end := strings.Index(fn, "\n}\n"); end >= 0 {
		fn = fn[:end]
	}
	if strings.Contains(fn, "innerHTML") {
		t.Errorf("showGuardrailToast must never touch innerHTML, got:\n%s", fn)
	}
	if !strings.Contains(fn, ".textContent") {
		t.Errorf("expected showGuardrailToast to render via textContent, got:\n%s", fn)
	}

	if !strings.Contains(js, `data.hit.severity === "danger"`) {
		t.Error(`expected the SSE message handler to gate on hit.severity === "danger" before toasting`)
	}
	if !strings.Contains(js, "showGuardrailToast(data.hit)") {
		t.Error("expected the SSE message handler to call showGuardrailToast(data.hit)")
	}
}
