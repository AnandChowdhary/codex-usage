package cli

import (
	"fmt"
	"io"
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

func (e *env) renderUsage(results []result, all bool) {
	st := e.styles()
	now := e.Now()
	labels := [2]string{
		commonLabel(results, func(r *usage.RateLimit) *usage.Window { return r.PrimaryWindow }, "5h"),
		commonLabel(results, func(r *usage.RateLimit) *usage.Window { return r.SecondaryWindow }, "weekly"),
	}
	t := &table{header: []string{
		"ACCOUNT", "PLAN",
		strings.ToUpper(labels[0]) + " LEFT", "RESETS",
		strings.ToUpper(labels[1]) + " LEFT", "RESETS",
		"NOTES",
	}}

	for _, r := range results {
		plan := r.account.PlanType
		var limit *usage.RateLimit
		if r.usage != nil {
			limit = r.usage.RateLimit
			if r.usage.PlanType != "" {
				plan = r.usage.PlanType
			}
		}
		row := []cell{{text: r.account.Label}, {text: orDash(plan)}}
		row = append(row, e.windowCells(limit, labels, now)...)
		row = append(row, e.notes(r, st))
		t.rows = append(t.rows, row)

		if !all || r.usage == nil {
			continue
		}
		for _, extra := range r.usage.AdditionalRateLimits {
			name := extra.LimitName
			if name == "" {
				name = extra.MeteredFeature
			}
			row := []cell{{text: "  ↳ " + name, style: st.dim}, {text: ""}}
			row = append(row, e.windowCells(extra.RateLimit, labels, now)...)
			note := cell{}
			if extra.RateLimit.Blocked() {
				note = cell{text: "limit reached", style: st.bad}
			}
			t.rows = append(t.rows, append(row, note))
		}
	}
	t.write(e.Stdout, st)
}

// windowCells renders the primary and secondary window as "left" and
// "resets" cells, naming the window when it differs from the column header.
func (e *env) windowCells(limit *usage.RateLimit, labels [2]string, now time.Time) []cell {
	st := e.styles()
	var cells []cell
	for i, w := range []*usage.Window{primary(limit), secondary(limit)} {
		if w == nil {
			cells = append(cells, cell{text: "-", style: st.dim}, cell{text: "-", style: st.dim})
			continue
		}
		left := w.LeftPercent()
		text := fmt.Sprintf("%.0f%%", left)
		if l := w.Label(); l != labels[i] {
			text += " (" + l + ")"
		}
		style := st.good
		switch {
		case left < 20:
			style = st.bad
		case left < 50:
			style = st.warn
		}
		cells = append(cells, cell{text: text, style: style}, cell{text: formatReset(now, w.ResetTime(now), e.Location)})
	}
	return cells
}

func (e *env) notes(r result, st styles) cell {
	if r.err != nil {
		return cell{text: r.err.Error(), style: st.bad}
	}
	var notes []string
	style := st.dim
	blocked := r.usage != nil && r.usage.RateLimit.Blocked()
	if u := r.usage; u != nil {
		if blocked {
			reason := "limit reached"
			if u.RateLimitReachedType != nil && u.RateLimitReachedType.Type != "" && u.RateLimitReachedType.Type != "rate_limit_reached" {
				reason = strings.ReplaceAll(u.RateLimitReachedType.Type, "_", " ")
			}
			notes = append(notes, reason)
			style = st.bad
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
		if !blocked {
			style = st.warn
		}
	}
	return cell{text: strings.Join(notes, "; "), style: style}
}

// commonLabel is the most frequent label of the chosen window, or fallback.
func commonLabel(results []result, pick func(*usage.RateLimit) *usage.Window, fallback string) string {
	counts := map[string]int{}
	best, bestCount := fallback, 0
	for _, r := range results {
		if r.usage == nil || r.usage.RateLimit == nil {
			continue
		}
		w := pick(r.usage.RateLimit)
		if w == nil {
			continue
		}
		l := w.Label()
		counts[l]++
		if counts[l] > bestCount {
			best, bestCount = l, counts[l]
		}
	}
	return best
}

func primary(r *usage.RateLimit) *usage.Window {
	if r == nil {
		return nil
	}
	return r.PrimaryWindow
}

func secondary(r *usage.RateLimit) *usage.Window {
	if r == nil {
		return nil
	}
	return r.SecondaryWindow
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
