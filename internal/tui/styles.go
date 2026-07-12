package tui

import "github.com/charmbracelet/lipgloss"

// Every color and lipgloss.Style the TUI renders with is built here, from
// named tokens copied verbatim from docs/ux/mock.html's CSS custom
// properties. This file (its token names) is frozen as of WP1
// (P3-design.md §2.6/§4): nothing outside styles.go constructs a
// lipgloss.Color or lipgloss.Style — that's what keeps the palette a single
// source of truth instead of hex codes sprinkled across every view file.
// Truecolor→256→16 degradation and NO_COLOR come free from lipgloss/termenv
// profile detection; tests force termenv.Ascii for determinism.
const (
	tokenTxt     = "#c9d4e0"
	tokenDim     = "#7d8996"
	tokenFaint   = "#4d5763"
	tokenAccent  = "#6cb6ff"
	tokenAccent2 = "#316dca"

	tokenAdd  = "#3fb950"
	tokenDel  = "#f85149"
	tokenWarn = "#e3b341"

	// ponytail: the mock's add/del/warn tints are translucent
	// (rgba(...,.1x) over --bg) — terminals have no alpha, so these are that
	// rgba flattened to a solid color over tokenBg. Revisit only if a real
	// terminal spot-check (design §5, ux-expert pass) says it reads wrong.
	tokenAddBg  = "#12251d"
	tokenDelBg  = "#29191d"
	tokenWarnBg = "#241f15"

	tokenBg     = "#0d1117"
	tokenBg2    = "#010409"
	tokenPanel  = "#0f141b"
	tokenPanel2 = "#131a23"
	tokenLine   = "#1f2731"
	tokenLine2  = "#2a343f"

	// Agent chip colors — same palette cmd/wt's ANSI output already uses.
	tokenChipClaude = "#d2a8ff"
	tokenChipCodex  = "#7ee787"
	tokenChipAider  = "#79c0ff"
	tokenChipGit    = "#8b949e"
)

// styleSet is every named style the TUI renders with, built once at
// package init (see the styles package var below).
type styleSet struct {
	Txt    lipgloss.Style // default body text
	Dim    lipgloss.Style // secondary text (times, paths, counts)
	Faint  lipgloss.Style // tertiary text (repo headers, idle dot)
	Accent lipgloss.Style // links/selection/branch names

	Add  lipgloss.Style
	Del  lipgloss.Style
	Warn lipgloss.Style

	AddBg  lipgloss.Style
	DelBg  lipgloss.Style
	WarnBg lipgloss.Style

	ChipClaude lipgloss.Style
	ChipCodex  lipgloss.Style
	ChipAider  lipgloss.Style
	ChipGit    lipgloss.Style

	Brand       lipgloss.Style // topbar "wt cockpit"
	RepoHeader  lipgloss.Style // sidebar group header
	SelectedRow lipgloss.Style // accent left border + brighter name
	Keybar      lipgloss.Style // bottom context bar
	Topbar      lipgloss.Style // top summary bar
	FatalCard   lipgloss.Style // protocol-mismatch / too-small full-screen card
	DownCard    lipgloss.Style // daemon-down-with-backoff full-screen card
}

func newStyles() styleSet {
	return styleSet{
		Txt:    lipgloss.NewStyle().Foreground(lipgloss.Color(tokenTxt)),
		Dim:    lipgloss.NewStyle().Foreground(lipgloss.Color(tokenDim)),
		Faint:  lipgloss.NewStyle().Foreground(lipgloss.Color(tokenFaint)),
		Accent: lipgloss.NewStyle().Foreground(lipgloss.Color(tokenAccent)),

		Add:  lipgloss.NewStyle().Foreground(lipgloss.Color(tokenAdd)),
		Del:  lipgloss.NewStyle().Foreground(lipgloss.Color(tokenDel)),
		Warn: lipgloss.NewStyle().Foreground(lipgloss.Color(tokenWarn)),

		AddBg:  lipgloss.NewStyle().Background(lipgloss.Color(tokenAddBg)),
		DelBg:  lipgloss.NewStyle().Background(lipgloss.Color(tokenDelBg)),
		WarnBg: lipgloss.NewStyle().Background(lipgloss.Color(tokenWarnBg)),

		ChipClaude: lipgloss.NewStyle().Foreground(lipgloss.Color(tokenChipClaude)),
		ChipCodex:  lipgloss.NewStyle().Foreground(lipgloss.Color(tokenChipCodex)),
		ChipAider:  lipgloss.NewStyle().Foreground(lipgloss.Color(tokenChipAider)),
		ChipGit:    lipgloss.NewStyle().Foreground(lipgloss.Color(tokenChipGit)),

		Brand:      lipgloss.NewStyle().Foreground(lipgloss.Color(tokenAccent)).Bold(true),
		RepoHeader: lipgloss.NewStyle().Foreground(lipgloss.Color(tokenFaint)),
		SelectedRow: lipgloss.NewStyle().Foreground(lipgloss.Color(tokenTxt)).Bold(true).
			BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(lipgloss.Color(tokenAccent)).
			BorderTop(false).BorderRight(false).BorderBottom(false),
		Keybar:    lipgloss.NewStyle().Foreground(lipgloss.Color(tokenDim)),
		Topbar:    lipgloss.NewStyle().Foreground(lipgloss.Color(tokenTxt)),
		FatalCard: lipgloss.NewStyle().Foreground(lipgloss.Color(tokenDel)).Bold(true).Padding(1, 2),
		DownCard:  lipgloss.NewStyle().Foreground(lipgloss.Color(tokenWarn)).Padding(1, 2),
	}
}

// styles is the single package-wide instance every view renders from.
var styles = newStyles()
