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

func wrapLines(text string, width int) []string {
	lines := []string{}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		runes := []rune(clean(line))
		for len(runes) > width {
			lines = append(lines, string(runes[:width]))
			runes = runes[width:]
		}
		lines = append(lines, string(runes))
	}
	return lines
}

func (a *app) style(s, code string) string {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (a *app) choose(title, description string, options []string) (int, error) {
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
		width = max(1, width-3)
		visible := min(len(options), 8, max(1, height-10))
		// Wrap descriptions rather than losing account identities on small screens.
		lines := wrapLines(description, width)
		pageSize := max(1, height-visible-8)
		offset = min(offset, max(0, len(lines)-pageSize))
		var screen strings.Builder
		line := func(s string) { fmt.Fprint(&screen, "\x1b[2K", s, "\r\n") }
		fmt.Fprint(&screen, "\x1b[H")
		line("  " + a.style(fit("COMMANDER  /  "+title, width), "1;36"))
		line("  " + a.style(strings.Repeat("─", min(width, 76)), "2"))
		line("")
		for _, s := range lines[offset:min(len(lines), offset+pageSize)] {
			line("  " + s)
		}
		if len(lines) > pageSize {
			line("  " + a.style(fit(fmt.Sprintf("PgUp/PgDn · details %d–%d of %d", offset+1, min(len(lines), offset+pageSize), len(lines)), width), "2"))
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
			label := fmt.Sprintf("%d  %s", i+1, labels[i])
			if i == selected {
				line("  " + a.style(fit("› "+label, width), "1;36"))
			} else {
				line("  " + fit("  "+label, width))
			}
		}
		line("")
		back := "Back"
		if title == "Home" {
			back = "Quit"
		}
		help := "↑↓/Tab Move   1–9 Jump   Enter Select   Esc " + back
		if width < 55 {
			help = "↑↓/Tab · Enter · Esc " + back
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
	if value == "" || len(data) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("enter a nonempty value of at most 1024 bytes")
	}
	if confirm {
		if utf8.RuneCount(data) < 4 {
			return "", errors.New("use at least 4 characters for the password")
		}
		fmt.Fprint(a.out, "  Repeat password (hidden): ")
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
			if len(value) >= 1024 {
				return nil, errors.New("hidden input exceeds 1024 bytes")
			}
			value = append(value, one[0])
		}
	}
}

func (a *app) pause(message string) { a.choose("Result", message, []string{"Continue"}) }
func (a *app) confirm(message string) bool {
	i, err := a.choose("Confirm", message, []string{"Cancel", "Confirm"})
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
		options = append(options, "New profile\tStart with an empty profile", "Import profile\tOpen a profile archive")
		if p.Active != nil {
			options = append(options, "Export profile\tShare the current profile")
		}
		if len(names) > 0 {
			options = append(options, "Rename profile", "Delete profile")
		}
		options = append(options, "Back")
		description := "Choose a profile to select its accounts."
		if len(names) == 0 {
			description = "Welcome. Create a profile or import an existing one.\nA profile groups the accounts you want to work with."
		}
		index, err := a.choose("Profiles", description, options)
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
		case "New profile":
			name, e := a.input("Profile name", "")
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
		case "Rename profile", "Delete profile":
			which, e := a.choose("Profiles / "+action, "Choose a profile.", names)
			if e != nil {
				continue
			}
			old := names[which]
			if action == "Rename profile" {
				name, e := a.input("New name", old)
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
			} else if a.confirm("Delete profile " + old + "? Saved logins remain available.") {
				err = a.editProfiles(func(p *profiles) error {
					delete(p.Profiles, old)
					if p.Active != nil && *p.Active == old {
						p.Active = nil
					}
					return nil
				})
			}
		case "Import profile":
			path, e := a.input("Archive path", "")
			if e == nil {
				err = a.transfer([]string{"import", path})
				if err == nil {
					a.pause("Profile imported. Select it to start working.")
				}
			}
		case "Export profile":
			err = a.transfer([]string{"export"})
			if err == nil {
				a.pause("Profile exported successfully.")
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
		width, _ := a.dimensions()
		rows := make([]account, len(members))
		for i, m := range members {
			rows[i] = account{member: m}
		}
		description := "Profile  " + name + "  ·  " + accountCount(len(members)) + "\n\n"
		if notice != "" {
			description += notice + "\n\n"
			notice = ""
		}
		if len(members) == 0 {
			description += "No accounts yet. Open Accounts to add your first login."
		} else {
			description += accountTable(rows, false, width-3)
			description += "\n\nSelected accounts · open Accounts to check login status."
		}
		index, err := a.choose("Home", description, []string{
			"Accounts\tView, add or remove accounts",
			"Profiles\tSwitch, manage or share profiles",
			"Quit\tReturn to your terminal",
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
			notice = "Could not complete the action: " + err.Error()
		}
	}
}

func (a *app) accountMenu(name string) error {
	for {
		_, members, err := a.profile(name)
		if err != nil {
			return err
		}
		width, _ := a.dimensions()
		if len(members) > 0 {
			fmt.Fprintln(a.out, "Checking account status…")
		}
		rows := a.resolve(members)
		description := "Profile  " + name + "\n\n" + accountTable(rows, true, width-3)
		for _, row := range rows {
			if row.State != "Active" && row.State != "Saved" {
				description += "\n\nSome accounts need attention. Use Add account to reconnect."
				break
			}
		}
		options := []string{"Add account\tUse a saved login or sign in"}
		if len(members) > 0 {
			options = append(options, "Remove account\tChoose an account to remove", "Refresh\tCheck login status again")
		}
		options = append(options, "Back")
		index, err := a.choose("Accounts", description, options)
		if err != nil {
			return err
		}
		action, _, _ := strings.Cut(options[index], "\t")
		switch action {
		case "Add account":
			err = a.addAccount(name)
		case "Remove account":
			err = a.removeAccount(name)
		case "Refresh":
			continue
		default:
			return nil
		}
		if err != nil && !errors.Is(err, errCancelled) {
			a.pause("Could not complete the action: " + err.Error())
		}
	}
}

func (a *app) addAccount(name string) error {
	labels := make([]string, len(services))
	for i, service := range services {
		labels[i] = serviceName(service)
	}
	which, err := a.choose("Accounts / Add account", "Choose a service.", labels)
	if err != nil {
		return err
	}
	service := services[which]
	for {
		rows, err := a.accounts(service)
		notice := "Choose a saved login or sign in."
		if err != nil {
			notice = "Could not read saved logins. Sign in to reconnect."
			rows = nil
		}
		table := [][]string{{"ACCOUNT", "CONTEXT", "STATUS"}}
		for _, r := range rows {
			context := r.Context
			if context == "" {
				context = "—"
			}
			table = append(table, []string{r.Account, context, r.State})
		}
		aligned := columns(table)
		options := aligned[1:]
		if len(rows) > 0 {
			notice += "\n\n     " + aligned[0]
		}
		options = append(options, "Sign in\tConnect another account")
		if service == "office" {
			options = append(options, "Set up another organization", "Repair permissions")
		}
		index, e := a.choose("Add account / "+serviceName(service), notice, options)
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
	i, err := a.choose("Accounts / Remove account", "Choose an account.", labels)
	if err != nil {
		return err
	}
	selected := members[i]
	action, err := a.choose("Remove account", selected.label()+"\n\nRemoving a saved login affects every profile that uses it.\nAzure removes all subscriptions of the sign in. Google revokes its token.", []string{"Remove from this profile\tKeep the saved login", "Remove saved login from all profiles", "Cancel"})
	if err != nil || action == 2 {
		return err
	}
	match := func(m member) bool {
		return m.id() == selected.id() || (action == 1 && m.Service == "azure" && selected.Service == "azure" && m.Account == selected.Account)
	}
	if action == 1 {
		if !a.confirm("Remove saved login " + selected.Account + " from all profiles?") {
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
		options = append(options, "Export selected logins", "Cancel")
		i, err := a.choose("Profiles / Export", "Enter toggles an account. Choose Export when ready.\nThe recipient receives the selected accounts' existing access.", options)
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
