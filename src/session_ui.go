package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
)

func (a *app) sessionMenu() error {
	for {
		entries, err := os.ReadDir(filepath.Join(a.root, ".sessions"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		sessions := []session{}
		options := []string{}
		for _, entry := range entries {
			if !sessionID.MatchString(entry.Name()) {
				continue
			}
			s, err := a.readSession(entry.Name())
			if err != nil {
				return err
			}
			sessions = append(sessions, s)
			options = append(options, s.Name+"\t"+accountCount(len(s.Accounts))+" · "+s.ID[:8])
		}
		options = append(options, "new session", "back")
		i, err := a.choose("sessions", "Each session has its own account and repository selection. Start your agent in its shell.", options)
		if err != nil {
			return err
		}
		if i == len(sessions)+1 {
			return nil
		}
		if i == len(sessions) {
			name, err := a.input("session name", "")
			if err != nil {
				return err
			}
			book, err := a.readProfiles()
			if err != nil {
				return err
			}
			names := []string{}
			for n := range book.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)
			args := []string{"new", name}
			if len(names) > 0 {
				choices := append([]string{"choose accounts individually"}, names...)
				n, err := a.choose("starting selection", "Optional: copy a saved selection, then adjust it for this session.", choices)
				if err != nil {
					return err
				}
				if n > 0 {
					args = append(args, names[n-1])
				}
			}
			if err = a.sessionCommand(args); err != nil {
				return err
			}
			continue
		}
		selected := sessions[i]
		action, err := a.choose("session / "+selected.Name, "Selections never switch another session's accounts.", []string{"open shell", "edit selection", "save as reusable selection", "close session", "back"})
		if err != nil {
			return err
		}
		switch action {
		case 0:
			err = a.sessionCommand([]string{"shell", selected.ID})
		case 1:
			err = a.sessionCommand([]string{"edit", selected.ID})
		case 2:
			var name string
			name, err = a.input("selection name", selected.Name)
			if err != nil {
				return err
			}
			book, e := a.readProfiles()
			if e != nil {
				return e
			}
			if _, ok := book.Profiles[name]; ok && !a.confirm("replace saved selection "+name+"?") {
				continue
			}
			err = a.editProfiles(func(p *profiles) error {
				p.Profiles[name] = selected.Accounts
				if p.Scopes == nil {
					p.Scopes = map[string][]string{}
				}
				p.Scopes[name] = selected.Repositories
				return nil
			})
		case 3:
			err = a.sessionCommand([]string{"close", selected.ID})
		}
		if err != nil && !errors.Is(err, errCancelled) {
			a.pause(err.Error())
		}
	}
}

func (a *app) accountNotInSession(selected member) error {
	entries, err := os.ReadDir(filepath.Join(a.root, ".sessions"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !sessionID.MatchString(entry.Name()) {
			continue
		}
		s, err := a.readSession(entry.Name())
		if err != nil {
			return err
		}
		for _, m := range s.Accounts {
			if m.id() == selected.id() || m.Service == "azure" && selected.Service == "azure" && m.Account == selected.Account {
				return errors.New("deselect this account in session " + s.Name + " before removing its login")
			}
		}
	}
	return nil
}
