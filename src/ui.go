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
	}
	return "", nil
}

func (a *app) choose(title, description string, options []string) (int, error) {
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
	selected := 0
	for {
		fmt.Fprint(a.out, "\x1b[H\x1b[2J")
		fmt.Fprintf(a.out, "\x1b[1;36mCOMMANDER\x1b[0m  /  %s\r\n\r\n", clean(title))
		if description != "" {
			fmt.Fprint(a.out, strings.ReplaceAll(description, "\n", "\r\n"), "\r\n\r\n")
		}
		_, height, _ := term.GetSize(int(a.in.Fd()))
		visible := height - 8 - strings.Count(description, "\n")
		if visible < 3 {
			visible = 3
		}
		if visible > 12 {
			visible = 12
		}
		start := 0
		if selected >= visible {
			start = selected - visible + 1
		}
		end := start + visible
		if end > len(options) {
			end = len(options)
		}
		for i := start; i < end; i++ {
			if i == selected {
				fmt.Fprintf(a.out, "  \x1b[36m› %s\x1b[0m\r\n", clean(options[i]))
			} else {
				fmt.Fprintf(a.out, "    %s\r\n", clean(options[i]))
			}
		}
		fmt.Fprint(a.out, "\r\n  ↑ ↓ Move   Enter Select   Esc Back\r\n")
		key, err := a.key()
		if err != nil {
			return -1, err
		}
		switch key {
		case "up", "k":
			selected = (selected + len(options) - 1) % len(options)
		case "down", "j":
			selected = (selected + 1) % len(options)
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
	fmt.Fprint(a.out, label)
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
	fmt.Fprint(a.out, label, " (hidden): ")
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
		fmt.Fprint(a.out, "Repeat password (hidden): ")
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

func (a *app) pause(message string) { a.choose("continue", message, []string{"Continue"}) }
func (a *app) confirm(message string) bool {
	i, err := a.choose("confirm", message, []string{"Cancel", "Confirm"})
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
		options := append([]string{}, names...)
		if p.Active != nil {
			for i, name := range names {
				if name == *p.Active {
					options[i] += " · active"
				}
			}
		}
		options = append(options, "New profile", "Rename profile", "Delete profile", "Import profile", "Back")
		index := len(names)
		if len(names) > 0 {
			index, err = a.choose("profiles", "Choose which accounts you want to work with.", options)
			if err != nil {
				return "", err
			}
		}
		if index < len(names) {
			name := names[index]
			err = a.editProfiles(func(p *profiles) error { p.Active = &name; return nil })
			return name, err
		}
		switch index - len(names) {
		case 0:
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
		case 1, 2:
			if len(names) == 0 {
				continue
			}
			which, e := a.choose("profiles", "", names)
			if e != nil {
				continue
			}
			old := names[which]
			if index-len(names) == 1 {
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
		case 3:
			path, e := a.input("Archive path", "")
			if e == nil {
				err = a.transfer([]string{"import", path})
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
		var description strings.Builder
		if notice != "" {
			fmt.Fprintln(&description, notice)
			notice = ""
		}
		if len(members) == 0 {
			description.WriteString("No accounts yet. Add a login to get started.\n")
		}
		for _, m := range members {
			fmt.Fprintln(&description, m.label())
		}
		description.WriteString("\nStart your agent yourself in this folder.")
		options := []string{"Add login", "Remove login", "Check accounts", "Profiles", "Export profile", "Tools and updates", "Quit"}
		index, err := a.choose(name, description.String(), options)
		if err != nil {
			return err
		}
		switch index {
		case 0:
			err = a.addAccount(name)
		case 1:
			err = a.removeAccount(name)
		case 2:
			var out strings.Builder
			old := a.out
			a.out = &out
			err = a.describe(name, false)
			a.out = old
			if err != nil {
				fmt.Fprintln(&out, err)
			}
			a.pause(out.String())
			err = nil
		case 3:
			var chosen string
			chosen, err = a.profilePicker()
			if err == nil {
				name = chosen
			}
		case 4:
			err = a.transfer([]string{"export"})
		case 5:
			which, e := a.choose("tools", "", []string{"Check installation", "Finish setup", "Update Commander and tools", "Back"})
			if e != nil {
				continue
			}
			switch which {
			case 0:
				var out strings.Builder
				old := a.out
				a.out = &out
				err = a.doctor()
				a.out = old
				if err != nil {
					fmt.Fprintln(&out, err)
				}
				a.pause(out.String())
				err = nil
			case 1:
				err = a.setup(false)
			case 2:
				err = a.update()
			}
		case 6:
			return nil
		}
		if err != nil && !errors.Is(err, errCancelled) {
			notice = err.Error()
		}
	}
}

func (a *app) addAccount(name string) error {
	which, err := a.choose("add login", "", services)
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
		options := []string{}
		for _, r := range rows {
			options = append(options, r.Account+" · "+r.Context+" · "+r.State)
		}
		options = append(options, "Sign in")
		if service == "office" {
			options = append(options, "Set up another organization", "Repair permissions")
		}
		index, e := a.choose(service, notice, options)
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
		labels = append(labels, m.label())
	}
	i, err := a.choose("remove login", "", labels)
	if err != nil {
		return err
	}
	selected := members[i]
	action, err := a.choose("remove login", selected.label()+"\nRemoving a saved login affects every profile that uses it.\nAzure removes all subscriptions of the sign in. Google revokes its token.", []string{"Remove from this profile", "Remove saved login from all profiles", "Cancel"})
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
			options = append(options, mark+m.label())
		}
		options = append(options, "Export selected logins", "Cancel")
		i, err := a.choose("export", "Select which logins to include. The recipient receives their existing access.", options)
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
