package tui

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/navbytes/wt-cockpit/internal/model"
)

// TestMain forces the ascii color profile for the whole package's test run
// (P3-design.md §2.6: "In tests, force termenv.Ascii for determinism") —
// styles.go's package-level `styles` var captures the default renderer's
// *pointer* once at init, so setting the profile here still reaches every
// already-built lipgloss.Style since SetColorProfile mutates that shared
// renderer in place rather than swapping it out.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

// fakeAPI is a test double for apiClient: no daemon, no socket — just
// canned responses, so app.go's Update()/View() logic (and conn.go's
// reconnect loop) are testable as pure functions per P3-design.md's WP1
// test list ("one teatest smoke: program starts with a fake client feeding
// fixtures").
type fakeAPI struct {
	mu sync.Mutex

	protocol   int
	version    string
	versionErr error

	worktrees    []model.Worktree
	worktreesErr error

	refreshErr error

	// events/errs are returned as-is by Events on every call — tests that
	// need distinct behavior per call (e.g. conn.go's reconnect loop) use
	// eventsFn instead.
	events   chan model.Event
	errs     chan error
	eventsFn func() (<-chan model.Event, <-chan error)

	eventsCalls int
}

func (f *fakeAPI) Version(context.Context) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.protocol, f.version, f.versionErr
}

func (f *fakeAPI) Worktrees(context.Context) ([]model.Worktree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.worktrees, f.worktreesErr
}

func (f *fakeAPI) Diff(context.Context, string) (model.Diff, error) {
	return model.Diff{}, nil
}

func (f *fakeAPI) SetReviewed(context.Context, string, string, bool, string) error {
	return nil
}

func (f *fakeAPI) Approve(context.Context, string) (model.ApproveResult, error) {
	return model.ApproveResult{}, nil
}

func (f *fakeAPI) Refresh(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshErr
}

func (f *fakeAPI) Events(ctx context.Context) (<-chan model.Event, <-chan error) {
	f.mu.Lock()
	f.eventsCalls++
	fn := f.eventsFn
	f.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return f.events, f.errs
}

// closedEvents returns a pair of already-closed channels: an Events() call
// that connects successfully but has nothing to say and ends immediately
// (a clean, empty stream) — err nil unless errVal is set.
func closedEvents(errVal error) (<-chan model.Event, <-chan error) {
	events := make(chan model.Event)
	errs := make(chan error, 1)
	close(events)
	errs <- errVal
	close(errs)
	return events, errs
}
