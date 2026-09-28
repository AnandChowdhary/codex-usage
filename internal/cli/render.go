package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AnandChowdhary/codex-usage/internal/usage"
)

// styles wraps text in ANSI colors when enabled.
type styles struct{ on bool }

func (a *App) styles() styles { return styles{on: a.Color} }

func (s styles) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (s styles) bold(t string) string   { return s.wrap("1", t) }
func (s styles) dim(t string) string    { return s.wrap("2", t) }
func (s styles) good(t string) string   { return s.wrap("32", t) }
func (s styles) warn(t string) string   { return s.wrap("33", t) }
func (s styles) bad(t string) string    { return s.wrap("31", t) }
func (s styles) accent(t string) string { return s.wrap("1;36", t) }

// cell is table text plus an optional style. Widths are measured on the plain
// text so colors don't break alignment.
type cell struct {
	text  string
	style func(string) string
}

type table struct {
	header []string
	rows   [][]cell
}

func (t *table) write(w io.Writer, st styles) {
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range t.rows {
		for i, c := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(c.text))
		}
	}
	line := func(cells []cell) {
		var b strings.Builder
		for i, c := range cells {
			text := c.text
			if c.style != nil {
				text = c.style(text)
			}
			b.WriteString(text)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c.text)+2))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	header := make([]cell, len(t.header))
	for i, h := range t.header {
		header[i] = cell{text: h, style: st.bold}
	}
	line(header)
	for _, row := range t.rows {
		line(row)
	}
}

// subLimit is an extra limit listed under its account with --all.
type subLimit struct {
	name  string
	limit *usage.RateLimit
}

func subLimits(u *usage.Response) []subLimit {
	var out []subLimit
	if u.CodeReviewRateLimit != nil {
		out = append(out, subLimit{name: "code review", limit: u.CodeReviewRateLimit})
	}
	for _, extra := range u.AdditionalRateLimits {
		name := extra.LimitName
		if name == "" {
			name = extra.MeteredFeature
		}
		out = append(out, subLimit{name: name, limit: extra.RateLimit})
	}
	return out
}

func (e *env) renderUsage(results []result, all bool) {
	st := e.styles()
	now := e.Now()

	// One column pair per window length, shortest first. Plans differ (Plus
	// has 5h and weekly windows, Pro may only have weekly), so columns follow
	// the windows actually reported rather than primary/secondary.
	seen := map[int64]bool{}
	var lengths []int64
	addWindows := func(limit *usage.RateLimit) {
		for _, w := range limit.Windows() {
			if !seen[w.LimitWindowSeconds] {
				seen[w.LimitWindowSeconds] = true
				lengths = append(lengths, w.LimitWindowSeconds)
			}
		}
	}
	for _, r := range results {
		if r.usage == nil {
			continue
		}
		addWindows(r.usage.RateLimit)
		if all {
			for _, s := range subLimits(r.usage) {
				addWindows(s.limit)
			}
		}
	}
	slices.Sort(lengths)

	header := []string{"ACCOUNT", "PLAN"}
	for _, l := range lengths {
		label := (&usage.Window{LimitWindowSeconds: l}).Label()
		header = append(header, strings.ToUpper(label)+" LEFT", "RESETS")
	}
	t := &table{header: append(header, "NOTES")}

	inCodex := e.codexKey()
	for _, r := range results {
		plan := r.account.PlanType
		var limit *usage.RateLimit
		if r.usage != nil {
			limit = r.usage.RateLimit
			if r.usage.PlanType != "" {
				plan = r.usage.PlanType
			}
		}
		row := []cell{{text: codexLabel(r.account.Label, r.account.Key() == inCodex)}, {text: orDash(plan)}}
		row = append(row, e.windowCells(limit, lengths, now)...)
		t.rows = append(t.rows, append(row, e.notes(r, st, now)))

		if !all || r.usage == nil {
			continue
		}
		for _, s := range subLimits(r.usage) {
			row := []cell{{text: "  ↳ " + s.name, style: st.dim}, {}}
			row = append(row, e.windowCells(s.limit, lengths, now)...)
			note := cell{}
			if s.limit.Blocked() {
				note = cell{text: "limit reached", style: st.bad}
			}
			t.rows = append(t.rows, append(row, note))
		}
	}
	t.write(e.Stdout, st)
}

// windowCells renders "left" and "resets" cells for each column's window length.
func (e *env) windowCells(limit *usage.RateLimit, lengths []int64, now time.Time) []cell {
	st := e.styles()
	var cells []cell
	for _, length := range lengths {
		var w *usage.Window
		for _, candidate := range limit.Windows() {
			if candidate.LimitWindowSeconds == length {
				w = candidate
				break
			}
		}
		if w == nil {
			cells = append(cells, cell{text: "-", style: st.dim}, cell{text: "-", style: st.dim})
			continue
		}
		left := w.LeftPercent()
		style := st.good
		switch {
		case left < 20:
			style = st.bad
		case left < 50:
			style = st.warn
		}
		cells = append(cells,
			cell{text: fmt.Sprintf("%.0f%%", left), style: style},
			cell{text: formatReset(now, w.ResetTime(now), e.Location)})
	}
	return cells
}

func (e *env) notes(r result, st styles, now time.Time) cell {
	if r.err != nil {
		return cell{text: r.err.Error(), style: st.bad}
	}
	var notes []string
	blocked, resetAvailable := false, false
	if u := r.usage; u != nil {
		if u.RateLimit.Blocked() {
			reason := "limit reached"
			if u.RateLimitReachedType != nil && u.RateLimitReachedType.Type != "" && u.RateLimitReachedType.Type != "rate_limit_reached" {
				reason = strings.ReplaceAll(u.RateLimitReachedType.Type, "_", " ")
			}
			notes = append(notes, reason)
			blocked = true
		}
		if u.SpendControl != nil && u.SpendControl.Reached {
			notes = append(notes, "spend limit reached")
			blocked = true
		}
		if n := resetCount(r); n > 0 {
			notes = append(notes, e.resetNote(n, r.resets, now))
			resetAvailable = true
		}
		if c := u.Credits; c != nil {
			switch {
			case c.Unlimited:
				notes = append(notes, "unlimited credits")
			case c.HasCredits && c.Balance != "":
				notes = append(notes, "credits: "+string(c.Balance))
			}
		}
		if u.RateLimit == nil {
			notes = append(notes, "no limits reported")
		}
	}
	if r.warning != nil {
		notes = append(notes, r.warning.Error())
	}

	style := st.dim
	switch {
	case blocked:
		style = st.bad
	case r.warning != nil:
		style = st.warn
	case resetAvailable:
		style = st.good
	}
	return cell{text: strings.Join(notes, "; "), style: style}
}

// resetCount is how many usage limit resets the account can redeem, preferring
// the detailed list when it was fetched.
func resetCount(r result) int64 {
	if r.resets != nil {
		return max(r.resets.AvailableCount, 0)
	}
	return r.usage.AvailableResets()
}

// resetNote describes available resets and when the first one expires.
func (e *env) resetNote(count int64, resets *usage.ResetCredits, now time.Time) string {
	noun := "usage limit resets"
	if count == 1 {
		noun = "usage limit reset"
	}
	text := fmt.Sprintf("%d %s available", count, noun)
	if available := resets.Available(); len(available) > 0 {
		if at, ok := available[0].Expiry(); ok {
			prefix := "expires"
			if count > 1 {
				prefix = "first expires"
			}
			text += " (" + prefix + " " + formatWhen(now, at, e.Location) + ")"
		}
	}
	return text
}

// formatReset shows a reset as a countdown within a day, else as a local time.
func formatReset(now, at time.Time, loc *time.Location) string {
	d := at.Sub(now)
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d < 7*24*time.Hour:
		return at.In(loc).Format("Mon 15:04")
	default:
		return at.In(loc).Format("Jan 2 15:04")
	}
}

// formatWhen is formatReset for use in a sentence: "in 38m" or "Thu 09:00".
func formatWhen(now, at time.Time, loc *time.Location) string {
	when := formatReset(now, at, loc)
	if d := at.Sub(now); d > 0 && d < 24*time.Hour {
		return "in " + when
	}
	return when
}

func formatAgo(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func formatRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// codexLabel marks the account the Codex CLI is signed in to.
func codexLabel(label string, inCodex bool) string {
	if inCodex {
		return label + " (codex)"
	}
	return label
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortID(id string) string {
	if utf8.RuneCountInString(id) <= 12 {
		return id
	}
	return string([]rune(id)[:12]) + "…"
}

func joinQuoted(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}
