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
	width = max(1, width)
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

func plainLines(text string, width int) []uiLine {
	lines := []uiLine{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		lines = append(lines, uiLine{{line, ""}}.wrap(width)...)
	}
	return lines
}

func statusLabel(state string) (string, string) {
	switch state {
	case "Active":
		return "● active", "32"
	case "Saved":
		return "○ saved", "32"
	case "":
		return "—", "2"
	default:
		return "! " + strings.ToLower(state), "33"
	}
}

func accountLines(rows []account, status bool, width int) []uiLine {
	if len(rows) == 0 {
		return plainLines("no accounts yet. choose add account to get started.", width)
	}
	if width > 0 && width < 64 {
		lines := []uiLine{}
		for i, r := range rows {
			if i > 0 {
				lines = append(lines, uiLine{})
			}
			label := uiLine{{serviceName(r.Service), "1"}}
			if status {
				text, tone := statusLabel(r.State)
				label = append(label, uiSpan{"  ", ""}, uiSpan{text, tone})
			}
			lines = append(lines, label.wrap(width)...)
			lines = append(lines, uiLine{{"  " + r.Account, ""}}.wrap(width)...)
			if r.Context != "" {
				lines = append(lines, uiLine{{"  context  ", "2"}, {r.Context, ""}}.wrap(width)...)
			}
		}
		return lines
	}
	headers := []string{"service", "account", "context"}
	if status {
		headers = append(headers, "status")
	}
	table := [][]string{headers}
	tones := [][]string{{"2", "2", "2", "2"}}
	for _, r := range rows {
		context := r.Context
		if context == "" {
			context = "—"
		}
		cells := []string{serviceName(r.Service), r.Account, context}
		colors := []string{"", "", "2"}
		if status {
			text, tone := statusLabel(r.State)
			cells = append(cells, text)
			colors = append(colors, tone)
		}
		table = append(table, cells)
		tones = append(tones, colors)
	}
	widths := make([]int, len(headers))
	for _, row := range table {
		for i, cell := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(clean(cell)))
		}
	}
	for width > 0 {
		total, widest := 2*(len(headers)-1), -1
		for i, size := range widths {
			total += size
			if size > len(headers[i]) && (widest < 0 || size > widths[widest]) {
				widest = i
			}
		}
		if total <= width || widest < 0 {
			break
		}
		widths[widest]--
	}
	lines := []uiLine{}
	for index, row := range table {
		cells := make([][]rune, len(row))
		for i, cell := range row {
			cells[i] = []rune(clean(cell))
		}
		for more := true; more; {
			more = false
			line := uiLine{}
			for i, cell := range cells {
				n := min(len(cell), widths[i])
				line = append(line, uiSpan{string(cell[:n]), tones[index][i]})
				if i < len(row)-1 {
					line = append(line, uiSpan{strings.Repeat(" ", widths[i]-n+2), ""})
				}
				cells[i] = cell[n:]
				more = more || len(cells[i]) > 0
			}
			lines = append(lines, line)
		}
		if index == 0 {
			parts := make([]string, len(widths))
			for i, w := range widths {
				parts[i] = strings.Repeat("─", w)
			}
			lines = append(lines, uiLine{{strings.Join(parts, "  "), "2"}})
		}
	}
	return lines
}

func accountView(name string, rows []account, status bool, notice string, width int) []uiLine {
	lines := uiLine{{"profile  ", "2"}, {name, "1"}, {"  ·  " + accountCount(len(rows)), "2"}}.wrap(width)
	lines = append(lines, uiLine{})
	if notice != "" {
		lines = append(lines, uiLine{{notice, "33"}}.wrap(width)...)
		lines = append(lines, uiLine{})
	}
	if len(rows) == 0 && !status {
		lines = append(lines, plainLines("no accounts yet. open accounts to add your first login.", width)...)
	} else {
		lines = append(lines, accountLines(rows, status, width)...)
		if len(rows) > 0 {
			hint := "selected accounts · open accounts to check logins"
			if status {
				attention := 0
				for _, r := range rows {
					if r.State != "Active" && r.State != "Saved" {
						attention++
					}
				}
				if attention == 0 {
					hint = "all accounts ready"
				} else {
					hint = fmt.Sprintf("%d need attention · add account to reconnect", attention)
				}
			}
			lines = append(lines, uiLine{})
			lines = append(lines, uiLine{{hint, "2"}}.wrap(width)...)
		}
	}
	return lines
}
