package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"unicode/utf8"

	"golang.org/x/term"
)

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
	case 'A':
		return "up", nil
	case 'B':
		return "down", nil
	case 'Z':
		return "up", nil
	case 'H':
		return "home", nil
	case 'F':
		return "end", nil
	case '5', '6':
		page := b[0]
		if _, err := a.in.Read(b); err != nil {
			return "", err
		}
		if b[0] == '~' {
			if page == '5' {
				return "pageup", nil
			}
			return "pagedown", nil
		}
	}
	return "", nil
}

// Columns contain display text only. Tabs align cells; terminal escapes and
// newlines in account labels must never become layout or terminal commands.
func columns(rows [][]string) []string {
	var out strings.Builder
	w := tabwriter.NewWriter(&out, 0, 0, 3, ' ', 0)
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = clean(cell)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush()
	return strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
}

func (a *app) dimensions() (int, int) {
	if f, ok := a.out.(*os.File); ok {
		if width, height, err := term.GetSize(int(f.Fd())); err == nil && width > 0 && height > 0 {
			return width, height
		}
	}
	return 80, 24
}

func fit(s string, width int) string {
	runes := []rune(clean(s))
	if len(runes) <= width {
		return string(runes)
	}
	if width < 1 {
		return ""
	}
	return string(runes[:width-1]) + "…"
}

func (a *app) style(s, code string) string {
	if !a.terminal() || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (a *app) choose(title, description string, options []string) (int, error) {
	return a.chooseView(title, func(width int) []uiLine { return plainLines(description, width) }, options)
}

func (a *app) chooseView(title string, content func(int) []uiLine, options []string) (int, error) {
	if len(options) == 0 {
		return -1, errCancelled
	}
	if !a.terminal() {
		return -1, errors.New("this action needs an interactive terminal")
	}
	state, err := term.MakeRaw(int(a.in.Fd()))
	if err != nil {
		return -1, err
	}
	defer term.Restore(int(a.in.Fd()), state)
	fmt.Fprint(a.out, "\x1b[?25l")
	defer fmt.Fprint(a.out, "\x1b[?25h")
	selected, offset := 0, 0
	rows := make([][]string, len(options))
	for i, option := range options {
		rows[i] = strings.Split(option, "\t")
	}
	labels := columns(rows)
	fmt.Fprint(a.out, "\x1b[H\x1b[2J")
	for {
		width, height := a.dimensions()
		width = max(1, min(108, width-4))
		visible := min(len(options), 8, max(1, height-10))
		// Wrap descriptions rather than losing account identities on small screens.
		lines := content(width)
		pageSize := max(1, height-visible-8)
		offset = min(offset, max(0, len(lines)-pageSize))
		var screen strings.Builder
		line := func(s string) { fmt.Fprint(&screen, "\x1b[2K", s, "\r\n") }
		fmt.Fprint(&screen, "\x1b[H")
		line("  " + a.renderLine(uiLine{{"commander", "1;36"}, {" / ", "2"}, {title, "1"}}, width))
		line("  " + a.style(strings.Repeat("─", width), "2"))
		line("")
		for _, s := range lines[offset:min(len(lines), offset+pageSize)] {
			line("  " + a.renderLine(s, width))
		}
		if len(lines) > pageSize {
			line("  " + a.style(fit(fmt.Sprintf("pgup/pgdn · details %d–%d of %d", offset+1, min(len(lines), offset+pageSize), len(lines)), width), "2"))
		}
		line("")
		start := 0
		if selected >= visible {
			start = selected - visible + 1
		}
		end := start + visible
		if end > len(options) {
			end = len(options)
		}
		for i := start; i < end; i++ {
			label := labels[i]
			first := rows[i][0]
			tail := strings.TrimPrefix(label, clean(first))
			prefix, tone := "  ", ""
			if i == selected {
				prefix, tone = "› ", "1;36"
			}
			line("  " + a.renderLine(uiLine{{prefix, tone}, {fmt.Sprintf("%d  ", i+1), "2"}, {first, tone}, {tail, "2"}}, width))
		}
		line("")
		back := "back"
		if title == "home" {
			back = "quit"
		}
		help := "↑↓/tab move   1–9 jump   enter select   esc " + back
		if width < 55 {
			help = "↑↓/tab · enter · esc " + back
		}
		if len(options) > visible {
			help += fmt.Sprintf("   %d/%d", selected+1, len(options))
		}
		fmt.Fprint(&screen, "\x1b[2K  ", a.style(fit(help, width), "2"), "\x1b[J")
		fmt.Fprint(a.out, screen.String())
		key, err := a.key()
		if err != nil {
			return -1, err
		}
		switch key {
		case "up", "k":
			selected = (selected + len(options) - 1) % len(options)
		case "down", "j", "\t":
			selected = (selected + 1) % len(options)
		case "home":
			selected = 0
		case "end":
			selected = len(options) - 1
		case "pageup":
			offset = max(0, offset-pageSize)
		case "pagedown":
			offset = min(max(0, len(lines)-pageSize), offset+pageSize)
		case "\r", "\n":
			fmt.Fprint(a.out, "\x1b[H\x1b[2J")
			return selected, nil
		case "escape", "\x03", "\x04", "q":
			fmt.Fprint(a.out, "\r\n")
			return -1, errCancelled
		default:
			if n, err := strconv.Atoi(key); err == nil && n > 0 && n <= len(options) {
				selected = n - 1
			}
		}
	}
}

func (a *app) input(label, value string) (string, error) {
	if !a.terminal() {
		return "", errors.New("this action needs an interactive terminal")
	}
	fmt.Fprint(a.out, "  ", label)
	if value != "" {
		fmt.Fprint(a.out, " [", clean(value), "]")
	}
	fmt.Fprint(a.out, ": ")
	var b strings.Builder
	one := make([]byte, 1)
	for {
		_, err := a.in.Read(one)
		if err != nil {
			return "", errCancelled
		}
		if one[0] == '\n' {
			break
		}
		if one[0] == '\r' {
			continue
		}
		if b.Len() > 4096 {
			return "", errors.New("input is too long")
		}
		b.WriteByte(one[0])
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		s = value
	}
	if s == "" {
		return "", errCancelled
	}
	return s, nil
}

func (a *app) password(label string, confirm bool) (string, error) {
	if !a.terminal() {
		return "", errors.New("hidden input needs an interactive terminal")
	}
	fmt.Fprint(a.out, "  ", label, " (hidden): ")
	data, err := a.readHidden()
	fmt.Fprintln(a.out)
	if err != nil {
		if errors.Is(err, errCancelled) {
			return "", errCancelled
		}
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
		fmt.Fprint(a.out, "  repeat password (hidden): ")
		second, err := a.readHidden()
		fmt.Fprintln(a.out)
		if err != nil || string(second) != value {
			return "", errors.New("passwords do not match")
		}
	}
	return value, nil
}

func (a *app) readHidden() ([]byte, error) {
	state, err := term.MakeRaw(int(a.in.Fd()))
	if err != nil {
		return nil, err
	}
	defer term.Restore(int(a.in.Fd()), state)
	value := []byte{}
	one := make([]byte, 1)
	for {
		if _, err := a.in.Read(one); err != nil {
			return nil, errCancelled
		}
		switch one[0] {
		case '\r', '\n':
			return value, nil
		case 3, 4:
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

func (a *app) pause(message string) { a.choose("result", message, []string{"continue"}) }
func (a *app) confirm(message string) bool {
	i, err := a.choose("confirm", message, []string{"cancel", "confirm"})
	return err == nil && i == 1
}

func (a *app) profilePicker() (string, error) {
	for {
		p, err := a.readProfiles()
		if err != nil {
			return "", err
		}
		names := make([]string, 0, len(p.Profiles))
		for name := range p.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		options := []string{}
		for _, name := range names {
			state := ""
			if p.Active != nil && name == *p.Active {
				state = " · current"
			}
			options = append(options, name+"\t"+accountCount(len(p.Profiles[name]))+state)
		}
		options = append(options, "new profile\tstart with an empty profile", "import profile\topen a profile archive")
		if p.Active != nil {
			options = append(options, "export profile\tshare the current profile")
		}
		if len(names) > 0 {
			options = append(options, "rename profile", "delete profile")
		}
		options = append(options, "back")
		description := "choose a profile to select its accounts."
		if len(names) == 0 {
			description = "welcome. create a profile or import an existing one.\na profile groups the accounts you want to work with."
		}
		index, err := a.choose("profiles", description, options)
		if err != nil {
			return "", err
		}
		if index < len(names) {
			name := names[index]
			err = a.editProfiles(func(p *profiles) error { p.Active = &name; return nil })
			return name, err
		}
		action, _, _ := strings.Cut(options[index], "\t")
		switch action {
		case "new profile":
			name, e := a.input("profile name", "")
			if e != nil {
				return "", e
			}
			err = a.editProfiles(func(p *profiles) error {
				if !validName(name) {
					return errors.New("use 1 to 80 visible characters")
				}
				if _, ok := p.Profiles[name]; ok {
					return errors.New("that profile already exists")
				}
				p.Profiles[name] = []member{}
				p.Active = &name
				return nil
			})
			if err == nil {
				return name, nil
			}
		case "rename profile", "delete profile":
			which, e := a.choose("profiles / "+action, "choose a profile.", names)
			if e != nil {
				continue
			}
			old := names[which]
			if action == "rename profile" {
				name, e := a.input("new name", old)
				if e != nil {
					continue
				}
				if name == old {
					continue
				}
				err = a.editProfiles(func(p *profiles) error {
					if !validName(name) {
						return errors.New("use 1 to 80 visible characters")
					}
					if _, ok := p.Profiles[name]; ok {
						return errors.New("that profile already exists")
					}
					p.Profiles[name] = p.Profiles[old]
					delete(p.Profiles, old)
					if p.Active != nil && *p.Active == old {
						p.Active = &name
					}
					return nil
				})
			} else if a.confirm("delete profile " + old + "? saved logins remain available.") {
				err = a.editProfiles(func(p *profiles) error {
					delete(p.Profiles, old)
					if p.Active != nil && *p.Active == old {
						p.Active = nil
					}
					return nil
				})
			}
		case "import profile":
			path, e := a.input("archive path", "")
			if e == nil {
				err = a.transfer([]string{"import", path})
				if err == nil {
					a.pause("profile imported. select it to start working.")
				}
			}
		case "export profile":
			err = a.transfer([]string{"export"})
			if err == nil {
				a.pause("profile exported successfully.")
			}
		default:
			return "", errCancelled
		}
		if err != nil && !errors.Is(err, errCancelled) {
			a.pause(err.Error())
		}
	}
}

func (a *app) menu() error {
	unlock, err := a.lock("menu")
	if err != nil {
		return err
	}
	defer unlock()
	p, err := a.readProfiles()
	if err != nil {
		return err
	}
	name := ""
	if p.Active != nil {
		name = *p.Active
	}
	if name == "" {
		name, err = a.profilePicker()
		if err != nil {
			return err
		}
	}
	notice := ""
	for {
		_, members, err := a.profile(name)
		if err != nil {
			name, err = a.profilePicker()
			if err != nil {
				return err
			}
			continue
		}
		rows := make([]account, len(members))
		for i, m := range members {
			rows[i] = account{member: m}
		}
		message := notice
		notice = ""
		index, err := a.chooseView("home", func(width int) []uiLine {
			return accountView(name, rows, false, message, width)
		}, []string{
			"accounts\tview, add or remove accounts",
			"profiles\tswitch, manage or share profiles",
			"quit\treturn to your terminal",
		})
		if err != nil {
			return err
		}
		switch index {
		case 0:
			err = a.accountMenu(name)
		case 1:
			var chosen string
			chosen, err = a.profilePicker()
			if err == nil {
				name = chosen
			}
		case 2:
			return nil
		}
		if err != nil && !errors.Is(err, errCancelled) {
			notice = "could not complete the action: " + err.Error()
		}
	}
}

func (a *app) accountMenu(name string) error {
	for {
		_, members, err := a.profile(name)
		if err != nil {
			return err
		}
		if len(members) > 0 {
			fmt.Fprintln(a.out, "checking accounts…")
		}
		rows := a.resolve(members)
		options := []string{"add account\tuse a saved login or sign in"}
		if len(members) > 0 {
			options = append(options, "remove account\tchoose an account to remove", "refresh\tcheck login status again")
		}
		options = append(options, "back")
		index, err := a.chooseView("accounts", func(width int) []uiLine { return accountView(name, rows, true, "", width) }, options)
		if err != nil {
			return err
		}
		action, _, _ := strings.Cut(options[index], "\t")
		switch action {
		case "add account":
			err = a.addAccount(name)
		case "remove account":
			err = a.removeAccount(name)
		case "refresh":
			continue
		default:
			return nil
		}
		if err != nil && !errors.Is(err, errCancelled) {
			a.pause("could not complete the action: " + err.Error())
		}
	}
}

func (a *app) addAccount(name string) error {
	labels := make([]string, len(services))
	for i, service := range services {
		labels[i] = serviceName(service)
	}
	which, err := a.choose("accounts / add account", "choose a service.", labels)
	if err != nil {
		return err
	}
	service := services[which]
	for {
		rows, err := a.accounts(service)
		var preparation *toolPreparationError
		if errors.As(err, &preparation) {
			return err
		}
		notice := "choose a saved login or sign in."
		if err != nil {
			notice = "could not read saved logins. sign in to reconnect."
			rows = nil
		}
		table := [][]string{{"account", "context", "status"}}
		for _, r := range rows {
			context := r.Context
			if context == "" {
				context = "—"
			}
			status, _ := statusLabel(r.State)
			table = append(table, []string{r.Account, context, status})
		}
		aligned := columns(table)
		options := aligned[1:]
		if len(rows) > 0 {
			notice += "\n\n     " + aligned[0]
		}
		options = append(options, "sign in\tconnect another account")
		if service == "office" {
			options = append(options, "set up another organization", "repair permissions")
		}
		index, e := a.choose("add account / "+serviceName(service), notice, options)
		if e != nil {
			return e
		}
		var selected member
		if index < len(rows) {
			selected = rows[index].member
		} else {
			err = nil
			unlock, e := a.lock("native")
			if e != nil {
				return e
			}
			if index == len(rows)+2 {
				_, err = a.run("m365", []string{"cli", "app", "reconsent", "--output", "text"}, false, nil)
				unlock()
				if err != nil {
					return err
				}
				continue
			}
			if index == len(rows)+1 {
				err = a.officeOrganization()
			}
			if err == nil {
				err = a.login(service)
			}
			unlock()
			if err != nil {
				return err
			}
			after, e := a.accounts(service)
			if e != nil {
				return e
			}
			before := map[string]bool{}
			for _, r := range rows {
				before[r.id()+r.Account] = true
			}
			added := []member{}
			for _, r := range after {
				if !before[r.id()+r.Account] {
					added = append(added, r.member)
				}
			}
			if len(added) != 1 {
				if len(after) == 0 {
					return errors.New("sign in returned no saved accounts")
				}
				continue
			}
			selected = added[0]
		}
		return a.editProfiles(func(p *profiles) error {
			members, ok := p.Profiles[name]
			if !ok {
				return errors.New("profile was removed")
			}
			next := []member{}
			for _, m := range members {
				if m.id() != selected.id() {
					next = append(next, m)
				}
			}
			p.Profiles[name] = append(next, selected)
			return nil
		})
	}
}

func (a *app) removeAccount(name string) error {
	_, members, err := a.profile(name)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	labels := []string{}
	for _, m := range members {
		labels = append(labels, serviceName(m.Service)+"\t"+m.Account+"\t"+m.Context)
	}
	i, err := a.choose("accounts / remove account", "choose an account.", labels)
	if err != nil {
		return err
	}
	selected := members[i]
	action, err := a.choose("remove account", selected.label()+"\n\nremoving a saved login affects every profile that uses it.\nazure removes all subscriptions of the sign in. google revokes its token.", []string{"remove from this profile\tkeep the saved login", "remove saved login from all profiles", "cancel"})
	if err != nil || action == 2 {
		return err
	}
	match := func(m member) bool {
		return m.id() == selected.id() || (action == 1 && m.Service == "azure" && selected.Service == "azure" && m.Account == selected.Account)
	}
	if action == 1 {
		if !a.confirm("remove saved login " + selected.Account + " from all profiles?") {
			return errCancelled
		}
		unlock, e := a.lock("native")
		if e != nil {
			return e
		}
		defer unlock()
		row := a.resolve([]member{selected})[0]
		if row.State != "Active" && row.State != "Saved" && row.State != "Sign in again" {
			return errors.New("cannot verify this login; remove it from the profile only or reconnect")
		}
		if err = a.deleteLogin(selected); err != nil {
			return err
		}
	}
	return a.editProfiles(func(p *profiles) error {
		for profile, entries := range p.Profiles {
			if action == 0 && profile != name {
				continue
			}
			next := []member{}
			for _, m := range entries {
				if !match(m) {
					next = append(next, m)
				}
			}
			p.Profiles[profile] = next
		}
		return nil
	})
}

func profileFilename(name string) string {
	name = strings.NewReplacer("/", "_", "\\", "_").Replace(name)
	for len(name) > 240 {
		name = name[:len(name)-1]
	}
	for !utf8.ValidString(name) {
		name = name[:len(name)-1]
	}
	return name + ".zip"
}

func (a *app) selectExport(members []member) ([]member, error) {
	if len(members) == 0 {
		return nil, errors.New("this profile has no logins to export")
	}
	selected := make([]bool, len(members))
	for i := range selected {
		selected[i] = true
	}
	for {
		options := []string{}
		for i, m := range members {
			mark := "○ "
			if selected[i] {
				mark = "✓ "
			}
			options = append(options, mark+serviceName(m.Service)+"\t"+m.Account+"\t"+m.Context)
		}
		options = append(options, "export selected logins", "cancel")
		i, err := a.choose("profiles / export", "enter toggles an account. choose export when ready.\nthe recipient receives the selected accounts' existing access.", options)
		if err != nil {
			return nil, err
		}
		if i < len(members) {
			selected[i] = !selected[i]
			continue
		}
		if i == len(members)+1 {
			return nil, errCancelled
		}
		result := []member{}
		for j, m := range members {
			if selected[j] {
				result = append(result, m)
			}
		}
		if len(result) == 0 {
			continue
		}
		return result, nil
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
