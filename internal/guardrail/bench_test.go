package guardrail

import (
	"fmt"
	"testing"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// bigDiffFixture builds a diff with ~5,000 added lines spread across several
// files — the eval-cost budget fixture named in P5-design.md §7 ("go test
// -bench guardrail eval on a 5k-line diff with the full default pack — budget
// < 5ms warm").
func bigDiffFixture() model.Diff {
	const files = 10
	const linesPerFile = 500 // 10 * 500 = 5,000 added lines total
	diff := model.Diff{}
	for f := 0; f < files; f++ {
		lines := make([]string, linesPerFile)
		for i := range lines {
			lines[i] = fmt.Sprintf("func g%d() int { return %d } // filler line content, nothing suspicious here", i, i)
		}
		diff.Files = append(diff.Files, addedFile(fmt.Sprintf("pkg%d/file.go", f), lines...))
	}
	return diff
}

// BenchmarkEvalDefaultPackOn5kLineDiff measures Eval's warm cost against the
// full shipped default rule set (including both content conditions,
// added_pattern and entropy) over a ~5,000-added-line diff. Budget: < 5ms/op
// warm (P5-design.md §7); report via `go test -bench BenchmarkEvalDefaultPackOn5kLineDiff ./internal/guardrail/`.
func BenchmarkEvalDefaultPackOn5kLineDiff(b *testing.B) {
	e := mustCompile(b, DefaultRules())
	diff := bigDiffFixture()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.Eval(diff)
	}
}
