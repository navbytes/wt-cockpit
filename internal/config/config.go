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
	"time"

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
	Roots     []string              `toml:"roots"`
	Base      string                `toml:"base"`
	Socket    string                `toml:"socket"`
	TCP       string                `toml:"tcp"`
	Web       string                `toml:"web"`
	State     string                `toml:"state"`
	Interval  time.Duration         `toml:"interval"`
	Watch     string                `toml:"watch"`
	LogFormat string                `toml:"log_format"`
	LogLevel  string                `toml:"log_level"`
	Repos     map[string]RepoConfig `toml:"repos"`
	Rules     []guardrail.Rule      `toml:"rules"`

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
	return cfg, nil
}
