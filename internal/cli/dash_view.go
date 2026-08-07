package cli

import (
	"fmt"
	"strings"

	"github.com/geetnsh2k1/pulse/internal/ui"
)

// The dashboard view: hand-rolled layout — a left column (functions,
// queues), a right log pane, an events strip, and a footer. Widths are
// computed from visible length (ANSI stripped), so styling never breaks
// alignment.

const leftW = 30

func (m *dashModel) View() string {
	if m.width < 60 || m.height < 14 {
		return "\n  terminal too small for the dashboard — enlarge it or use `pulse list`\n"
	}

	var b strings.Builder
	b.WriteString(m.header() + "\n")

	// Body: left column beside the log pane.
	bodyH := m.height - 2 /*header+footer*/ - m.eventsHeight()
	left := m.leftColumn(bodyH)
	right := m.logPane(bodyH, m.width-leftW-1)
	for i := 0; i < bodyH; i++ {
		b.WriteString(padVisible(lineAt(left, i), leftW) + " " + lineAt(right, i) + "\n")
	}

	for _, l := range m.eventsStrip() {
		b.WriteString(l + "\n")
	}
	b.WriteString(m.footer())
	return b.String()
}

func (m *dashModel) header() string {
	status := ui.OK("● live")
	if m.dataErr != "" {
		status = ui.Err("● " + m.dataErr)
	}
	left := fmt.Sprintf("%s %s · %s · %s", ui.AccentBold("⚡ pulse"), ui.Bold(m.project), status, ui.Dim("api "+m.info.APIAddr))
	return left
}

func (m *dashModel) leftColumn(h int) []string {
	var out []string
	out = append(out, ui.AccentBold("functions"))
	for _, fn := range m.functions {
		counts := ui.Dim(fmt.Sprintf("%d✓", fn.ok))
		if fn.bad > 0 {
			counts += " " + ui.Err(fmt.Sprintf("%d✗", fn.bad))
		}
		out = append(out, " "+padVisible(ui.Fn(fn.Name), 18)+" "+counts)
	}

	out = append(out, "", ui.AccentBold("queues"))
	if len(m.queues) == 0 {
		out = append(out, ui.Dim(" none declared"))
	}
	for _, q := range m.queues {
		depth := fmt.Sprintf("%d·%d·%d", q.Visible, q.InFlight, q.Delayed)
		switch {
		case strings.HasSuffix(q.Name, "-dlq") && q.Visible > 0:
			depth = ui.Err(depth + " !")
		case q.Visible+q.InFlight+q.Delayed == 0:
			depth = ui.Dim(depth)
		}
		out = append(out, " "+padVisible(ui.Bold(q.Name), 18)+" "+depth)
	}
	if len(out) > h {
		out = out[:h]
	}
	return out
}

func (m *dashModel) logPane(h, w int) []string {
	title := ui.AccentBold("logs")
	if m.filtering {
		title += " " + ui.Accent("/"+m.filter+"▌")
	} else if m.filter != "" {
		title += " " + ui.Accent("/"+m.filter) + ui.Dim(" (esc clears)")
	} else {
		title += ui.Dim(" — / filters")
	}
	out := []string{title}

	lines := m.logLines
	if m.filter != "" {
		lines = nil
		for _, l := range m.logLines {
			if strings.Contains(stripANSI(l), m.filter) {
				lines = append(lines, l)
			}
		}
	}
	rows := h - 1
	end := len(lines) - m.logScroll
	if end > len(lines) {
		end = len(lines)
	}
	start := max(0, end-rows)
	for _, l := range lines[start:max(start, end)] {
		out = append(out, truncVisible(l, w))
	}
	if len(lines) == 0 {
		out = append(out, ui.Dim(" waiting for activity — curl a route or `pulse send` something"))
	}
	return out
}

func (m *dashModel) eventsHeight() int { return 2 + min(len(m.events), 4) }

func (m *dashModel) eventsStrip() []string {
	title := ui.AccentBold("events")
	if m.focusEvents {
		title += ui.Dim(" — ↑↓ select · Enter replays")
	} else {
		title += ui.Dim(" — tab to focus")
	}
	out := []string{title}
	if len(m.events) == 0 {
		return append(out, ui.Dim(" nothing recorded yet"))
	}
	for i, ev := range m.events {
		if i >= 4 {
			break
		}
		cursor := "  "
		if m.focusEvents && i == m.selected {
			cursor = ui.Accent("▸ ")
		}
		outcome := ui.OK(ev.Status)
		if ev.Status != "success" {
			outcome = ui.Err(orDefault(ev.Status, "?"))
		}
		out = append(out, fmt.Sprintf("%s%s %s %s %s %s · %s", cursor,
			ui.Bold(shortEventID(ev.ID)), ui.Dim(fmtEventTime(ev.CreatedAt)),
			ui.Cyan(ev.Type), ui.Dim("→"), ui.Fn(ev.Function), outcome))
	}
	return out
}

func (m *dashModel) footer() string {
	if m.toast != "" {
		return m.toast + ui.Dim("   · q quit")
	}
	return ui.Dim("q quit · tab focus events · ↑↓ scroll/select · Enter replay · / filter")
}

// ---- visible-width helpers (ANSI-aware) ----

func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func visibleLen(s string) int { return len([]rune(stripANSI(s))) }

func padVisible(s string, w int) string {
	if d := w - visibleLen(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// truncVisible cuts a styled line to w visible columns, closing any open
// style so truncation can't bleed color into the next line.
func truncVisible(s string, w int) string {
	if visibleLen(s) <= w {
		return s
	}
	var b strings.Builder
	visible, inEsc := 0, false
	for _, r := range s {
		b.WriteRune(r)
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			visible++
			if visible >= w-1 {
				b.WriteString("…\x1b[0m")
				return b.String()
			}
		}
	}
	return b.String()
}
