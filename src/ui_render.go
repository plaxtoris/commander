package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Styling is separate from text. Account labels never become terminal escapes,
// and width calculations always use the original, unstyled text.
type uiSpan struct{ text, tone string }
type uiLine []uiSpan

func (line uiLine) plain() string {
	var out strings.Builder
	for _, span := range line {
		out.WriteString(clean(span.text))
	}
	return out.String()
}

func (line uiLine) wrap(width int) []uiLine {
	if width <= 0 {
		return []uiLine{line}
	}
	lines := []uiLine{}
	current := uiLine{}
	used := 0
	for _, span := range line {
		runes := []rune(clean(span.text))
		for len(runes) > 0 {
			n := min(len(runes), width-used)
			current = append(current, uiSpan{string(runes[:n]), span.tone})
			used += n
			runes = runes[n:]
			if used == width {
				lines = append(lines, current)
				current = uiLine{}
				used = 0
			}
		}
	}
	if used > 0 || len(lines) == 0 {
		lines = append(lines, current)
	}
	return lines
}

func (a *app) renderLine(line uiLine, width int) string {
	var out strings.Builder
	remaining := max(0, width)
	truncated := utf8.RuneCountInString(line.plain()) > remaining
	if truncated && remaining > 0 {
		remaining--
	}
	for _, span := range line {
		runes := []rune(clean(span.text))
		n := min(len(runes), remaining)
		if n > 0 {
			text := string(runes[:n])
			if span.tone != "" {
				text = a.style(text, span.tone)
			}
			out.WriteString(text)
			remaining -= n
		}
	}
	if truncated && width > 0 {
		out.WriteString(a.style("…", "2"))
	}
	return out.String()
}

func textWidth(s string) int { return utf8.RuneCountInString(clean(s)) }

func pad(s string, n int) string { return clean(s) + strings.Repeat(" ", max(0, n-textWidth(s))) }

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func statusLabel(state string) (string, string) {
	switch state {
	case "Ready":
		return "✓ ready", "32"
	case "":
		return "—", "2"
	default:
		return "! " + strings.ToLower(state), "33"
	}
}

// Account lines group accounts under their service with aligned columns.
// Narrow terminals stack each account instead of truncating its identity.
func accountLines(rows []account, limit int) []uiLine {
	if len(rows) == 0 {
		return []uiLine{{{"no accounts yet", "2"}}}
	}
	cells := make([][3]string, len(rows))
	widths := [3]int{}
	for i, r := range rows {
		name, context := r.display()
		status, _ := statusLabel(r.State)
		cells[i] = [3]string{name, context, status}
		for j, cell := range cells[i] {
			widths[j] = max(widths[j], textWidth(cell))
		}
	}
	wide := limit <= 0 || 4+widths[0]+2+widths[1]+2+widths[2] <= limit
	lines := []uiLine{}
	for _, service := range services {
		header := true
		for i, r := range rows {
			if r.Service != service {
				continue
			}
			if header {
				lines = append(lines, uiLine{{serviceName(service), "1"}})
				header = false
			}
			c := cells[i]
			_, tone := statusLabel(r.State)
			if wide {
				lines = append(lines, uiLine{{"    " + pad(c[0], widths[0]) + "  ", ""}, {pad(c[1], widths[1]) + "  ", "2"}, {c[2], tone}})
				continue
			}
			lines = append(lines, uiLine{{"    " + c[0], ""}}.wrap(limit)...)
			if c[1] != "" {
				lines = append(lines, uiLine{{"      " + c[1], "2"}}.wrap(limit)...)
			}
			lines = append(lines, uiLine{{"      " + c[2], tone}}.wrap(limit)...)
		}
	}
	return lines
}

// GroupLines wraps between items, never inside one; a width of zero never
// wraps. Labels share one column, so every session aligns alike.
func groupLines(groups []group, width int) []uiLine {
	labels := 13
	for _, g := range groups {
		labels = max(labels, textWidth(g.label))
	}
	labels += 2
	lines := []uiLine{}
	for _, g := range groups {
		line, used := uiLine{{pad(g.label, labels), "2"}}, labels
		for i, item := range g.items {
			size := textWidth(item.plain())
			if i > 0 && width > 0 && used+3+size > width {
				lines = append(lines, line)
				line, used = uiLine{{strings.Repeat(" ", labels), ""}}, labels
			} else if i > 0 {
				line, used = append(line, uiSpan{" · ", "2"}), used+3
			}
			line, used = append(line, item...), used+size
		}
		lines = append(lines, line)
	}
	return lines
}

// Key hints render "[k]ey" with the bracketed part highlighted and wrap
// between hints when the terminal is narrow.
func hints(keys string, limit int) []uiLine {
	lines := []uiLine{{}}
	used := 0
	for _, hint := range strings.Split(keys, "  ") {
		if hint == "" {
			continue
		}
		size := textWidth(hint)
		if used > 0 && used+2+size > limit {
			lines = append(lines, uiLine{})
			used = 0
		}
		line := &lines[len(lines)-1]
		if used > 0 {
			*line = append(*line, uiSpan{"  ", ""})
			used += 2
		}
		for hint != "" {
			before, rest, found := strings.Cut(hint, "[")
			key, after, closed := strings.Cut(rest, "]")
			if !found || !closed {
				*line = append(*line, uiSpan{hint, "2"})
				break
			}
			*line = append(*line, uiSpan{before, "2"}, uiSpan{"[" + key + "]", "1"})
			hint = after
		}
		used += size
	}
	return lines
}
