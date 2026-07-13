package guardrail

import (
	"crypto/sha1"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// packFileName is the frozen, checked-in per-repo override filename, read
// only from a repo's main worktree root (P5-design.md §1.3) — never from a
// feature worktree, so an agent working in one cannot weaken the guardrails
// judging its own diff.
const packFileName = ".wtcockpit.toml"

// Resolver resolves the effective rule engine for a repo, folding in that
// repo's optional .wtcockpit.toml pack. It holds the compiled global engine
// (config [[rules]] or DefaultRules()) plus a per-repo cache invalidated by
// the pack file's mtime+size — one os.Stat per worktree per refresh,
// negligible (P5-design.md §1.3's "Mechanics").
type Resolver struct {
	globalEngine *Engine
	globalRules  []Rule // globalEngine.Rules(): the final, auto-named set Merge folds packs into
	globalSource string // "default" | "global"

	mu    sync.Mutex
	cache map[string]*resolverEntry // repoPath -> last-resolved result
}

// resolverEntry is one repo's cached resolution: the engine to Eval against,
// its provenance-tagged rule list (for Effective), and enough pack file
// identity to know when to recompute.
type resolverEntry struct {
	modTime time.Time
	size    int64

	engine     *Engine
	effective  []RuleWithSource
	packPath   string // "" when no pack is in effect (absent or unreadable)
	packStatus string // "none" | "ok" | "error: <msg>"

	warnedHash string // content hash of the last malformed pack body we logged, for dedup
}

// NewResolver compiles the global rule set once (source is "default" or
// "global", for Effective's provenance tag) and returns a Resolver ready for
// For/Effective. An error here means the global rules themselves are
// invalid; config.Load already validates via Compile before this is ever
// called in production, so this is a defensive check, not a load-bearing one.
func NewResolver(globalRules []Rule, source string) (*Resolver, error) {
	eng, err := Compile(globalRules)
	if err != nil {
		return nil, err
	}
	return &Resolver{
		globalEngine: eng,
		globalRules:  eng.Rules(),
		globalSource: source,
		cache:        map[string]*resolverEntry{},
	}, nil
}

// For returns the compiled rule engine to evaluate a diff from repoPath
// against: the global engine when no pack exists there, or the pack-merged
// engine otherwise. A malformed pack fails closed to the global engine —
// this never returns nil and never disables guardrails outright.
func (r *Resolver) For(repoPath string) *Engine {
	return r.resolve(repoPath).engine
}

// Effective returns worktreeID's owning repo's fully-resolved, provenance-
// tagged rule set — GET /api/rules and `wt rules`'s payload.
func (r *Resolver) Effective(worktreeID, repoPath string) Effective {
	e := r.resolve(repoPath)
	return Effective{
		WorktreeID: worktreeID,
		RepoPath:   repoPath,
		PackPath:   e.packPath,
		PackStatus: e.packStatus,
		Rules:      e.effective,
	}
}

// Stats reports how many currently-cached repos are running a validly-loaded
// pack ("loaded") vs a malformed one that fell back to global ("errors") —
// the source for statusPayload's additive `rulePacks` field.
func (r *Resolver) Stats() (loaded, errs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.cache {
		switch {
		case e.packStatus == "ok":
			loaded++
		case strings.HasPrefix(e.packStatus, "error:"):
			errs++
		}
	}
	return loaded, errs
}

// resolve returns repoPath's current cached resolution, recomputing it if the
// pack file's presence/mtime/size changed since the last look.
func (r *Resolver) resolve(repoPath string) *resolverEntry {
	packPath := filepath.Join(repoPath, packFileName)
	info, statErr := os.Stat(packPath)

	r.mu.Lock()
	defer r.mu.Unlock()

	prev, hadPrev := r.cache[repoPath]

	if statErr != nil {
		if hadPrev && prev.packPath == "" {
			return prev // already resolved to "no pack"; nothing changed
		}
		entry := &resolverEntry{engine: r.globalEngine, effective: r.taggedGlobal(), packStatus: "none"}
		r.cache[repoPath] = entry
		return entry
	}

	if hadPrev && prev.packPath == packPath && prev.modTime.Equal(info.ModTime()) && prev.size == info.Size() {
		return prev // unchanged since last look
	}

	entry := r.loadPack(repoPath, packPath, info, prev)
	r.cache[repoPath] = entry
	return entry
}

// loadPack reads and applies repoPath's pack file. On any failure (unreadable,
// malformed TOML, unknown key, or a Compile error on the merged set) it fails
// closed to the global engine and logs one warning per (repo, pack content) —
// a standing typo must not spam every refresh cycle, but a NEW bad edit (or
// the same typo reappearing in a different repo) still gets logged.
func (r *Resolver) loadPack(repoPath, packPath string, info os.FileInfo, prev *resolverEntry) *resolverEntry {
	data, err := os.ReadFile(packPath)
	if err != nil {
		// Vanished between Stat and ReadFile: treat exactly like "no pack".
		return &resolverEntry{engine: r.globalEngine, effective: r.taggedGlobal(), packStatus: "none"}
	}
	hash := contentHash(data)
	entry := &resolverEntry{modTime: info.ModTime(), size: info.Size(), packPath: packPath, warnedHash: hash}

	pack, perr := ParsePack(data)
	if perr == nil {
		merged := Merge(r.globalRules, r.globalSource, pack)
		rules := make([]Rule, len(merged))
		for i, m := range merged {
			rules[i] = m.Rule
		}
		eng, cerr := Compile(rules)
		if cerr == nil {
			entry.engine = eng
			final := eng.Rules()
			tagged := make([]RuleWithSource, len(final))
			for i, rr := range final {
				tagged[i] = RuleWithSource{Rule: rr, Source: merged[i].Source}
			}
			entry.effective = tagged
			entry.packStatus = "ok"
			return entry
		}
		perr = cerr // a pack that parses fine but breaks the combined set is the same failure class
	}

	entry.engine = r.globalEngine
	entry.effective = r.taggedGlobal()
	entry.packStatus = "error: " + perr.Error()
	if prev == nil || prev.warnedHash != hash {
		slog.Warn("guardrail: malformed .wtcockpit.toml pack; falling back to global rules", "repo", repoPath, "path", packPath, "error", perr)
	}
	return entry
}

func (r *Resolver) taggedGlobal() []RuleWithSource {
	tagged := make([]RuleWithSource, len(r.globalRules))
	for i, rr := range r.globalRules {
		tagged[i] = RuleWithSource{Rule: rr, Source: r.globalSource}
	}
	return tagged
}

func contentHash(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}
