package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/term"
)

var errTerminal = errors.New("this action needs an interactive terminal")

func (a *app) terminal() bool {
	f, ok := a.out.(*os.File)
	return ok && term.IsTerminal(int(a.in.Fd())) && term.IsTerminal(int(f.Fd()))
}

func (a *app) key() (string, error) {
	b := make([]byte, 1)
	if _, err := a.in.Read(b); err != nil {
		return "", err
	}
	if b[0] != 27 {
		return string(b), nil
	}
	var set syscall.FdSet
	fd := int(a.in.Fd())
	if fd >= len(set.Bits)*64 {
		return "escape", nil
	}
	set.Bits[fd/64] |= 1 << uint(fd%64)
	timeout := syscall.Timeval{Usec: 50000}
	n, _ := syscall.Select(fd+1, &set, nil, nil, &timeout)
	if n == 0 {
		return "escape", nil
	}
	if _, err := a.in.Read(b); err != nil {
		return "", err
	}
	if b[0] != '[' {
		return "escape", nil
	}
	if _, err := a.in.Read(b); err != nil {
		return "", err
	}
	switch b[0] {
	case 'A', 'Z':
		return "up", nil
	case 'B':
		return "down", nil
	case 'H':
		return "home", nil
	case 'F':
		return "end", nil
	case '5', '6':
		page := b[0]
		if _, err := a.in.Read(b); err != nil {
			return "", err
		}
		if b[0] == '~' && page == '5' {
			return "pageup", nil
		}
		if b[0] == '~' {
			return "pagedown", nil
		}
	}
	return "", nil
}

func (a *app) dimensions() (int, int) {
	if f, ok := a.out.(*os.File); ok {
		if width, height, err := term.GetSize(int(f.Fd())); err == nil && width > 0 && height > 0 {
			return width, height
		}
	}
	return 80, 24
}

func (a *app) style(s, code string) string {
	if !a.terminal() || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (a *app) clear() {
	if a.terminal() {
		fmt.Fprint(a.out, "\x1b[H\x1b[2J")
	}
}

func (a *app) raw() (func(), error) {
	if !a.terminal() {
		return nil, errTerminal
	}
	fd := int(a.in.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { term.Restore(fd, state) }, nil
}

// A row is one selectable line, or a header when pick is false. More lines
// follow below it, aligned with its content.
type row struct {
	line   uiLine
	more   []uiLine
	indent int
	pick   bool
}

type view struct {
	title, info, keys string
	notice, tone      string
	rows              []row
}

func heading(title string) uiLine {
	line := uiLine{{"commander", "1;36"}}
	if title != "" {
		line = append(line, uiSpan{" · " + title, "1"})
	}
	return line
}

// List draws a full height screen with the key hints at the bottom and
// returns the pressed key with the selected row. It handles movement itself.
// The cursor stays at the hints, so a prompt can replace them.
func (a *app) list(v view, at int) (string, int, error) {
	restore, err := a.raw()
	if err != nil {
		return "", -1, err
	}
	defer restore()
	fmt.Fprint(a.out, "\x1b[?25l\x1b[H\x1b[2J")
	defer fmt.Fprint(a.out, "\x1b[?25h")
	picks := []int{}
	cur := 0
	for i, r := range v.rows {
		if r.pick {
			if i <= at {
				cur = len(picks)
			}
			picks = append(picks, i)
		}
	}
	offset := 0
	for {
		columns, height := a.dimensions()
		limit := max(1, min(120, columns-4))
		selected := -1
		if len(picks) > 0 {
			selected = picks[cur]
		}
		body := []uiLine{}
		first, last := 0, 0
		for i, r := range v.rows {
			indent := strings.Repeat(" ", r.indent)
			line := uiLine{{indent, ""}}
			content := r.line
			if r.pick {
				mark := uiSpan{"  ", ""}
				if i == selected {
					mark = uiSpan{"› ", "1;36"}
					content = highlight(content)
					first = len(body)
				}
				line = append(line, mark)
			}
			body = append(body, append(line, content...))
			for _, more := range r.more {
				body = append(body, append(uiLine{{indent + "  ", ""}}, more...))
			}
			if i == selected {
				last = len(body) - 1
			}
		}
		top := []uiLine{heading(v.title), {{strings.Repeat("─", limit), "2"}}, {}}
		if v.notice != "" {
			tone := v.tone
			if tone == "" {
				tone = "33"
			}
			top = append(top, uiLine{{v.notice, tone}}.wrap(limit)...)
			top = append(top, uiLine{})
		}
		foot := hints(v.keys, limit)
		room := max(1, height-len(top)-len(foot)-1)
		info := v.info
		if len(body) > room {
			info = strings.TrimSpace(fmt.Sprintf("%s  %d/%d", info, cur+1, len(picks)))
		}
		if gap := limit - textWidth(top[0].plain()) - textWidth(info); info != "" && gap >= 2 {
			top[0] = append(top[0], uiSpan{strings.Repeat(" ", gap) + info, "2"})
		}
		offset = min(offset, first)
		offset = max(offset, last-room+1)
		offset = max(0, min(offset, len(body)-room))
		var screen strings.Builder
		screen.WriteString("\x1b[H")
		put := func(line uiLine) { screen.WriteString("\x1b[2K  " + a.renderLine(line, limit) + "\r\n") }
		for _, line := range top {
			put(line)
		}
		for i := offset; i < offset+room; i++ {
			if i < len(body) {
				put(body[i])
			} else {
				put(nil)
			}
		}
		put(nil)
		for i, line := range foot {
			screen.WriteString("\x1b[2K  " + a.renderLine(line, limit))
			if i < len(foot)-1 {
				screen.WriteString("\r\n")
			}
		}
		screen.WriteString("\x1b[J")
		if len(foot) > 1 {
			fmt.Fprintf(&screen, "\x1b[%dA", len(foot)-1)
		}
		screen.WriteString("\r")
		fmt.Fprint(a.out, screen.String())
		key, err := a.key()
		if err != nil {
			return "", -1, err
		}
		switch key {
		case "up", "k":
			cur = max(0, cur-1)
		case "down", "j", "\t":
			cur = min(max(0, len(picks)-1), cur+1)
		case "home":
			cur = 0
		case "end", "G":
			cur = max(0, len(picks)-1)
		case "pageup":
			cur = max(0, cur-room)
		case "pagedown":
			cur = min(max(0, len(picks)-1), cur+room)
		case "":
		default:
			if name := map[string]string{"\r": "enter", "\n": "enter", " ": "space", "escape": "esc", "\x03": "esc", "\x04": "esc"}[key]; name != "" {
				key = name
			}
			return key, selected, nil
		}
	}
}

// The selected row turns its plain and bold text cyan; dim details stay dim.
func highlight(line uiLine) uiLine {
	out := make(uiLine, len(line))
	for i, span := range line {
		if span.tone == "" || span.tone == "1" {
			span.tone = "1;36"
		}
		out[i] = span
	}
	return out
}

func mark(on bool) uiSpan {
	if on {
		return uiSpan{"[x]", "32"}
	}
	return uiSpan{"[ ]", "2"}
}

// Prompt edits one line in place of the key hints. Escape or an empty line
// cancels.
func (a *app) prompt(label, value string) (string, error) {
	restore, err := a.raw()
	if err != nil {
		return "", err
	}
	defer restore()
	text := []byte(value)
	for {
		columns, _ := a.dimensions()
		shown := []rune(clean(string(text)))
		if room := max(1, columns-6-textWidth(label)); len(shown) > room {
			shown = append([]rune("…"), shown[len(shown)-room+1:]...)
		}
		fmt.Fprint(a.out, "\r\x1b[J  ", a.style(label+":", "1"), " ", string(shown), "\x1b[?25h")
		key, err := a.key()
		if err != nil {
			return "", errCancelled
		}
		switch key {
		case "\r", "\n":
			fmt.Fprint(a.out, "\r\n")
			s := strings.TrimSpace(string(text))
			if s == "" || !utf8.ValidString(s) {
				return "", errCancelled
			}
			return s, nil
		case "escape", "\x03", "\x04":
			fmt.Fprint(a.out, "\r\n")
			return "", errCancelled
		case "\x7f", "\b":
			if len(text) > 0 {
				_, size := utf8.DecodeLastRune(text)
				text = text[:len(text)-size]
			}
		case "\x15":
			text = text[:0]
		default:
			if len(key) == 1 && key[0] >= 32 && len(text) < 4096 {
				text = append(text, key[0])
			}
		}
	}
}

// Ask replaces the key hints with a yes or no question; only y confirms.
func (a *app) ask(question string) bool {
	restore, err := a.raw()
	if err != nil {
		return false
	}
	defer restore()
	fmt.Fprint(a.out, "\r\x1b[J  ", a.style(question, "1"), " ", a.style("[y/N]", "2"), " ")
	key, err := a.key()
	fmt.Fprint(a.out, "\r\n")
	return err == nil && (key == "y" || key == "Y")
}

func (a *app) password(label string, confirm bool) (string, error) {
	read := func(label string) ([]byte, error) {
		fmt.Fprint(a.out, "\r\x1b[J  ", a.style(label+":", "1"), " ")
		data, err := a.readHidden()
		fmt.Fprint(a.out, "\r\n")
		return data, err
	}
	data, err := read(label)
	if errors.Is(err, errCancelled) || errors.Is(err, errTerminal) {
		return "", err
	}
	if err != nil {
		return "", errors.New("hidden input is unavailable")
	}
	value := string(data)
	if value == "" || len(data) > 8192 || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("enter a nonempty value of at most 8192 bytes")
	}
	if confirm {
		if utf8.RuneCount(data) < 4 {
			return "", errors.New("use at least 4 characters for the password")
		}
		second, err := read("repeat " + label)
		if err != nil || string(second) != value {
			return "", errors.New("passwords do not match")
		}
	}
	return value, nil
}

func (a *app) readHidden() ([]byte, error) {
	restore, err := a.raw()
	if err != nil {
		return nil, err
	}
	defer restore()
	value := []byte{}
	one := make([]byte, 1)
	for {
		if _, err := a.in.Read(one); err != nil {
			return nil, errCancelled
		}
		switch one[0] {
		case '\r', '\n':
			return value, nil
		case 3, 4, 27:
			return nil, errCancelled
		case 8, 127:
			if len(value) > 0 {
				_, size := utf8.DecodeLastRune(value)
				value = value[:len(value)-size]
			}
		default:
			if len(value) >= 8192 {
				return nil, errors.New("hidden input exceeds 8192 bytes")
			}
			value = append(value, one[0])
		}
	}
}

// Keep absolute paths readable in the menu and relative paths relative to cwd.
func archivePath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, path[2:])
	}
	return filepath.Abs(path)
}
