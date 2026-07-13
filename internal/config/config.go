// Package config loads wtd's optional TOML config file
// (~/.config/wtcockpit/config.toml by default). It only parses and validates —
// it knows nothing about flags. Precedence (explicit flags > config > built-in
// defaults) is resolved by the caller (cmd/wtd), which knows which flags the
// user actually passed.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/navbytes/wt-cockpit/internal/guardrail"
)

// RepoConfig is a per-repo override, keyed by the repo's absolute path in
// Config.Repos: `[repos."/abs/path"] base = "develop"`.
type RepoConfig struct {
	Base string `toml:"base"`
}

// NotificationsConfig is [notifications]'s parsed shape (P5-design.md §1.5,
// §2): the desktop-notifier's enable switch, severity floor, and per-key
// re-notify cooldown. EnabledSet distinguishes an absent [notifications]
// table (or a present one that just never sets `enabled`) from an explicit
// `enabled = false` — both decode Enabled to Go's bool zero value, but only
// the latter should actually turn notifications off; see EnabledOr.
type NotificationsConfig struct {
	Enabled  bool          `toml:"enabled"`
	Severity string        `toml:"severity"`
	Cooldown time.Duration `toml:"cooldown"`

	EnabledSet bool `toml:"-"`
}

// EnabledOr resolves the frozen default: notifications are on unless the
// config explicitly says `enabled = false` (P5-design.md §1.5 — "Default ON
// because danger hits are rare by design").
func (n NotificationsConfig) EnabledOr() bool {
	if !n.EnabledSet {
		return true
	}
	return n.Enabled
}

// SeverityOr resolves the frozen default floor: "danger" only, unless the
// config sets "warn" (which admits warn+danger).
func (n NotificationsConfig) SeverityOr() string {
	if n.Severity == "" {
		return "danger"
	}
	return n.Severity
}

// CooldownOr resolves the frozen default: 10 minutes.
func (n NotificationsConfig) CooldownOr() time.Duration {
	if n.Cooldown <= 0 {
		return 10 * time.Minute
	}
	return n.Cooldown
}

// Config is the on-disk shape of config.toml. The zero value (returned when
// the file doesn't exist) means "nothing configured" — every field left unset.
type Config struct {
	Roots []string `toml:"roots"`
	// IncludeRepos/ExcludeRepos filter which discovered repos the daemon
	// actually watches, matched against the repo folder's BASENAME (what
	// shows as the repo name everywhere in the UI) using stdlib
	// path/filepath.Match glob syntax only (internal/discovery.DiscoverRepos
	// applies the actual include/exclude semantics). See Load's validation
	// below for why a bad pattern is a load-time error rather than a
	// silent no-op.
	IncludeRepos []string              `toml:"include_repos"`
	ExcludeRepos []string              `toml:"exclude_repos"`
	Base         string                `toml:"base"`
	Socket       string                `toml:"socket"`
	TCP          string                `toml:"tcp"`
	Web          string                `toml:"web"`
	State        string                `toml:"state"`
	Interval     time.Duration         `toml:"interval"`
	Watch        string                `toml:"watch"`
	LogFormat    string                `toml:"log_format"`
	LogLevel     string                `toml:"log_level"`
	Repos        map[string]RepoConfig `toml:"repos"`
	Rules        []guardrail.Rule      `toml:"rules"`

	// RulesSet is true iff [[rules]] appeared in the file at all (even empty).
	// An absent [[rules]] means "use guardrail.DefaultRules()"; a present one —
	// however short — replaces the defaults outright, because append-only
	// would make the defaults un-disableable.
	RulesSet bool `toml:"-"`

	Notifications NotificationsConfig `toml:"notifications"`
}

// RulesOr returns the config's own rules if [[rules]] was present in the file,
// or defaults otherwise.
func (c Config) RulesOr(defaults []guardrail.Rule) []guardrail.Rule {
	if c.RulesSet {
		return c.Rules
	}
	return defaults
}

// DefaultPath returns the conventional config location: $XDG_CONFIG_HOME/
// wtcockpit/config.toml, or ~/.config/wtcockpit/config.toml if unset.
func DefaultPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "wtcockpit", "config.toml")
}

// Load reads and parses the config file at path. A missing file is not an
// error: it returns the zero Config, so callers fall back to built-in
// defaults. Malformed TOML, or a key this version doesn't recognise (a typo in
// a hand-edited guardrails file is a real footgun otherwise), is an error.
// [[rules]], when present, is also routed through guardrail.Compile so a
// self-contradictory hand-edited rule (bad severity, a non-compiling
// added_pattern, ...) fails fast at load time too — the same "typo fails
// fast" philosophy, extended from the TOML shape to the rule semantics.
// [notifications].severity is validated as an enum here too; .cooldown's
// "parseable duration" half of P5-design.md §1.5's "Validated at load" is
// already enforced by BurntSushi's own time.Duration decoding (same as the
// existing top-level `interval` field) — a malformed string like "abc"
// fails at the toml.DecodeFile call above, before this function ever sees it.
func Load(path string) (Config, error) {
	var cfg Config
	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("%s: unknown config key %q", path, undecoded[0])
	}
	cfg.RulesSet = meta.IsDefined("rules")
	if cfg.RulesSet {
		if _, err := guardrail.Compile(cfg.Rules); err != nil {
			return Config{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	cfg.Notifications.EnabledSet = meta.IsDefined("notifications", "enabled")
	switch cfg.Notifications.Severity {
	case "", "danger", "warn":
	default:
		return Config{}, fmt.Errorf("%s: invalid notifications.severity %q (want \"danger\" or \"warn\")", path, cfg.Notifications.Severity)
	}
	if err := validateRepoGlobs(path, "include_repos", cfg.IncludeRepos); err != nil {
		return Config{}, err
	}
	if err := validateRepoGlobs(path, "exclude_repos", cfg.ExcludeRepos); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateRepoGlobs checks that every pattern in patterns is syntactically
// valid per stdlib path/filepath.Match's own grammar — fail-closed, same as
// the rest of Load's validation: an invalid pattern must fail the daemon's
// startup, not silently misbehave at scan time.
//
// This used to probe filepath.Match(pat, "x") directly, but Match only
// surfaces ErrBadPattern LAZILY: it parses a pattern chunk by chunk and
// gives up as soon as an earlier segment fails to match the specific name
// it was called with, so probing against "x" passed clean for a pattern
// like "prod-*[" — the mismatch on the literal "prod-" happens before the
// dangling "[" is ever parsed — yet that same pattern errors at scan time
// against a real repo named e.g. "prod-api" (internal/discovery.anyGlobMatch
// would then swallow that error as "no match": exclude fails OPEN, include
// silently drops wanted repos). checkGlobSyntax below walks the WHOLE
// pattern unconditionally instead — no "name" involved at all — so this is
// caught regardless of what name a later scan would ever try it against.
func validateRepoGlobs(path, field string, patterns []string) error {
	for _, pat := range patterns {
		if err := checkGlobSyntax(pat); err != nil {
			return fmt.Errorf("%s: invalid %s pattern %q: %w", path, field, pat, err)
		}
	}
	return nil
}

// checkGlobSyntax reports whether pattern is syntactically well-formed per
// path/filepath.Match's documented grammar (see that stdlib package's
// match.go, which this mirrors): a linear syntax walk, not a matcher —
// O(len(pattern)), independent of any "name" being matched. The grammar's
// only two error shapes (from Match's own scanChunk/matchChunk/getEsc) are
// a malformed "[...]" character class (empty, a bare "-"/"]" where a range
// item was expected, or running out of pattern before the closing "]") and
// a dangling "\" with nothing left to escape. Escaping is disabled on
// Windows — filepath.Match's own documented behavior ("\\" is the path
// separator there instead) — so those checks are skipped on that GOOS, to
// stay exactly in sync with what Match will actually do at scan time on
// whatever OS wtd is actually running on.
func checkGlobSyntax(pattern string) error {
	escape := runtime.GOOS != "windows"
	i := 0
	for i < len(pattern) {
		switch pattern[i] {
		case '[':
			var err error
			if i, err = skipGlobClass(pattern, i, escape); err != nil {
				return err
			}
		case '\\':
			if !escape {
				i++
				continue
			}
			if i+1 >= len(pattern) {
				return filepath.ErrBadPattern
			}
			i += 2 // the backslash plus exactly one escaped byte
		default:
			i++
		}
	}
	return nil
}

// skipGlobClass parses one "[...]" character class starting at
// pattern[start] (which must be '['), mirroring filepath.Match's own
// matchChunk '[' case: an optional leading "^", then one or more range
// items (each optionally "lo-hi"), until a "]" closes it. Returns the index
// just past that closing "]", or filepath.ErrBadPattern for anything
// matchChunk/getEsc would themselves reject — including running out of
// pattern before a "]" is ever found (the "unterminated class" case).
func skipGlobClass(pattern string, start int, escape bool) (int, error) {
	i := start + 1 // past '['
	if i < len(pattern) && pattern[i] == '^' {
		i++
	}
	nrange := 0
	for {
		if i < len(pattern) && pattern[i] == ']' && nrange > 0 {
			return i + 1, nil
		}
		var err error
		if i, err = skipGlobClassItem(pattern, i, escape); err != nil {
			return 0, err
		}
		if i < len(pattern) && pattern[i] == '-' {
			if i, err = skipGlobClassItem(pattern, i+1, escape); err != nil {
				return 0, err
			}
		}
		nrange++
	}
}

// skipGlobClassItem parses one character-range endpoint, mirroring
// filepath.Match's own getEsc EXACTLY: a bare "-" or "]" (or running out of
// pattern) can't start an item; "\" (when escaping applies) is stripped,
// erroring if nothing follows it; either way (escaped or not) getEsc then
// decodes one full UTF-8 RUNE from whatever remains and rejects
// utf8.RuneError — FIX 7: this used to short-circuit the escaped branch as
// exactly one BYTE, which over-rejected a legal escaped multibyte character
// in a class (e.g. "[\€]") as though its continuation bytes were their own,
// invalid, standalone item.
func skipGlobClassItem(pattern string, i int, escape bool) (int, error) {
	if i >= len(pattern) || pattern[i] == '-' || pattern[i] == ']' {
		return 0, filepath.ErrBadPattern
	}
	if escape && pattern[i] == '\\' {
		i++
		if i >= len(pattern) {
			return 0, filepath.ErrBadPattern
		}
	}
	r, n := utf8.DecodeRuneInString(pattern[i:])
	if r == utf8.RuneError && n == 1 {
		return 0, filepath.ErrBadPattern
	}
	return i + n, nil
}
