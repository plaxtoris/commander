package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

const homeKeys = "[enter] open  [n]ew  [e]dit  [r]ename  [d]elete  [i]mport  e[x]port  [q]uit"
const editKeys = "[space] select  [a]dd account  [x] delete login  [enter] save  [esc] cancel"

// The menu lists sessions. Every action is one key; names and confirmations
// replace the key hints instead of opening another screen.
func (a *app) menu() error {
	title := ""
	if a.session != nil {
		title = "session " + a.session.Name
	}
	admin := a.admin()
	notice, tone, focus, at := "", "", admin.lastSession(), 0
	for {
		all, err := admin.sessions()
		if err != nil {
			return err
		}
		names := 0
		for _, s := range all {
			names = max(names, min(24, textWidth(s.Name)))
		}
		// One calm line per session; the selected one lists its accounts below.
		rows := []row{}
		for i, s := range all {
			used := strings.Join(s.serviceNames(), " · ")
			if used == "" {
				used = "no accounts"
			}
			r := row{pick: true, line: uiLine{{pad(s.Name, names), "1"}, {"   " + used, "2"}}}
			r.detail = func(width int) []uiLine { return groupLines(s.groups(), width) }
			if s.ID == focus {
				at = i
			}
			rows = append(rows, r)
		}
		if len(all) == 0 {
			rows = append(rows, row{line: uiLine{{"no sessions yet", "2"}}})
		}
		rows = append(rows, row{pick: true, line: uiLine{{"+ new session", "36"}}})
		key, i, err := admin.list(view{title: title, info: count(len(all), "session"), keys: homeKeys, notice: notice, tone: tone, rows: rows}, at)
		if err != nil {
			return err
		}
		notice, tone, focus, at = "", "32", "", i
		picked := len(all) > 0 && i < len(all)
		var s session
		if picked {
			s = all[i]
		}
		switch {
		case key == "q" || key == "esc":
			admin.clear()
			return nil
		case key == "i":
			notice, err = admin.importArchive("")
		case key == "n" || key == "enter" && !picked:
			s, err = admin.createSession()
			notice, focus = s.Name+" created", s.ID
		case !picked:
		case key == "x":
			notice, err = admin.exportArchive(s)
		case key == "enter":
			admin.clear()
			err = admin.sessionShell(s)
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				err = nil
			}
		case key == "e":
			err = admin.editSession(s)
			notice = s.Name + " saved"
		case key == "r":
			var name string
			if name, err = admin.prompt("rename "+s.Name, s.Name); err == nil && name != s.Name {
				err = admin.renameSession(s, name)
				notice = "renamed to " + name
			}
		case key == "d":
			err = errCancelled
			if admin.ask("delete session " + s.Name + "? logins stay connected.") {
				err = admin.deleteSession(s)
				notice = s.Name + " deleted"
			}
		}
		if err != nil {
			notice, tone = err.Error(), ""
			if errors.Is(err, errCancelled) {
				notice = ""
			}
		}
	}
}

func (a *app) createSession() (session, error) {
	name, err := a.prompt("name", "")
	if err != nil {
		return session{}, err
	}
	if err = a.checkName(name, ""); err != nil {
		return session{}, err
	}
	accounts, repos, err := a.editSelection(name, nil, nil)
	if err != nil {
		return session{}, err
	}
	return a.newSession(name, accounts, repos)
}

func (a *app) editSession(s session) error {
	accounts, repos, err := a.editSelection(s.Name, s.Accounts, s.Repositories)
	if err != nil {
		return err
	}
	return a.updateSession(s.ID, func(current *session) error {
		current.Accounts, current.Repositories = accounts, repos
		return nil
	})
}

func (a *app) loading(title string) {
	if a.terminal() {
		a.clear()
		a.out.Write([]byte("  " + a.renderLine(heading(title), 120) + "\r\n\r\n  " + a.style("loading accounts…", "2") + "\r\n"))
	}
}

// What an edit screen row stands for.
type target struct {
	login int
	scope string
	add   bool
}

// EditSelection shows every connected login under its service, the
// repositories of the ticked GitHub logins and an add row. It returns the
// ticked accounts and repositories. Ticked accounts that are no longer
// connected stay visible until they are unticked.
func (a *app) editSelection(title string, accounts []member, repos []string) ([]member, []string, error) {
	// Logins are added and removed in the global store, even from a session shell.
	a = a.admin()
	chosen, ticked := map[string]bool{}, map[string]bool{}
	for _, m := range accounts {
		chosen[m.id()] = true
	}
	for _, r := range repos {
		ticked[strings.ToLower(r)] = true
	}
	owners := map[string][]string{}
	var logins []account
	var failed map[string]string
	notice, tone, at, reload := "", "", 0, true
	for {
		if reload {
			a.loading(title)
			logins, failed = a.logins()
			known := map[string]bool{}
			for _, r := range logins {
				known[r.id()] = true
			}
			for _, m := range accounts {
				if !known[m.id()] {
					logins = append(logins, account{member: m, State: "Missing account"})
				}
			}
			reload = false
		}
		scopes := []string{}
		add := func(scope string) {
			for _, s := range scopes {
				if strings.EqualFold(s, scope) {
					return
				}
			}
			scopes = append(scopes, scope)
		}
		for _, r := range logins {
			if r.Service != "github" || !chosen[r.id()] {
				continue
			}
			if _, ok := owners[r.id()]; !ok && ready(r.State) {
				a.loading(title)
				found, err := a.githubOwners(r.member)
				if err != nil {
					_, user := r.github()
					notice, tone = "could not load the organizations of "+user, ""
				}
				owners[r.id()] = found
			}
			for _, owner := range owners[r.id()] {
				add(owner)
			}
		}
		for _, r := range repos {
			add(r)
		}
		rows, targets := []row{}, []target{}
		header := func(text string) {
			rows, targets = append(rows, row{line: uiLine{{text, "1"}}}), append(targets, target{login: -1})
		}
		names, contexts := 0, 0
		for _, r := range logins {
			name, context := r.display()
			names, contexts = max(names, min(32, textWidth(name))), max(contexts, min(28, textWidth(context)))
		}
		for _, service := range services {
			first := true
			for i, r := range logins {
				if r.Service != service {
					continue
				}
				if first {
					header(serviceName(service))
					first = false
				}
				name, context := r.display()
				line := uiLine{mark(chosen[r.id()]), {" " + pad(name, names) + "  ", ""}, {pad(context, contexts), "2"}}
				if !ready(r.State) {
					status, color := statusLabel(r.State)
					line = append(line, uiSpan{"  " + status, color})
				}
				rows, targets = append(rows, row{indent: 2, pick: true, line: line}), append(targets, target{login: i})
			}
			if failed[service] != "" {
				if first {
					header(serviceName(service))
				}
				status, color := statusLabel(failed[service])
				rows, targets = append(rows, row{indent: 4, line: uiLine{{status, color}}}), append(targets, target{login: -1})
			}
			if service == "github" && len(scopes) > 0 {
				header("repositories")
				for _, scope := range scopes {
					line := uiLine{mark(ticked[strings.ToLower(scope)]), {" " + scope, ""}}
					rows, targets = append(rows, row{indent: 2, pick: true, line: line}), append(targets, target{login: -1, scope: scope})
				}
			}
		}
		if len(logins) == 0 {
			rows, targets = append(rows, row{line: uiLine{{"no accounts yet", "2"}}}), append(targets, target{login: -1})
		}
		rows, targets = append(rows, row{pick: true, line: uiLine{{"+ add account", "36"}}}), append(targets, target{login: -1, add: true})
		key, i, err := a.list(view{title: title, keys: editKeys, notice: notice, tone: tone, rows: rows}, at)
		if err != nil {
			return nil, nil, err
		}
		notice, tone, at = "", "", i
		t := targets[i]
		switch {
		case key == "esc" || key == "q":
			return nil, nil, errCancelled
		case key == "a" || key == "enter" && t.add:
			added, err := a.addLogin()
			if err != nil && !errors.Is(err, errCancelled) {
				notice = err.Error()
			}
			for _, m := range added {
				chosen[m.id()] = true
			}
			reload = len(added) > 0 || err == nil
		case key == "enter":
			selected, scoped := []member{}, []string{}
			for _, r := range logins {
				if chosen[r.id()] {
					selected = append(selected, r.member)
				}
			}
			for _, scope := range scopes {
				if ticked[strings.ToLower(scope)] {
					scoped = append(scoped, scope)
				}
			}
			return selected, scoped, nil
		case key == "space" && t.login >= 0:
			id := logins[t.login].id()
			chosen[id] = !chosen[id]
		case key == "space" && t.scope != "":
			ticked[strings.ToLower(t.scope)] = !ticked[strings.ToLower(t.scope)]
		case key == "x" && t.login >= 0:
			r := logins[t.login]
			err = a.removeLogin(r)
			if err == nil {
				delete(chosen, r.id())
				accounts = without(accounts, func(m member) bool { return m.id() == r.id() })
				notice, tone, reload = "login deleted", "32", true
			} else if !errors.Is(err, errCancelled) {
				notice = err.Error()
			}
		}
	}
}

func without(members []member, drop func(member) bool) []member {
	kept := []member{}
	for _, m := range members {
		if !drop(m) {
			kept = append(kept, m)
		}
	}
	return kept
}

// GitHubOwners lists the login itself and its organizations as owner/*.
func (a *app) githubOwners(m member) ([]string, error) {
	host, user := m.github()
	owners := []string{user + "/*"}
	root, extra, finish, err := a.accountRuntime(m)
	if err != nil {
		return owners, err
	}
	defer os.RemoveAll(root)
	data, runErr := a.run("gh", []string{"api", "--hostname", host, "--paginate", "user/orgs", "--jq", ".[].login"}, true, extra)
	if err = errors.Join(runErr, finish()); err != nil {
		return owners, err
	}
	for _, org := range strings.Fields(string(data)) {
		if validRepositoryScope(org + "/*") {
			owners = append(owners, org+"/*")
		}
	}
	return owners, nil
}

// AddLogin signs in to a service and returns the accounts it added.
func (a *app) addLogin() ([]member, error) {
	loginChoices := [][2]string{}
	for _, service := range services {
		loginChoices = append(loginChoices, [2]string{service, serviceName(service)})
	}
	loginChoices = append(loginChoices, [2]string{"office setup", serviceName("office") + " · new organization"}, [2]string{"office repair", serviceName("office") + " · repair permissions"})
	rows := []row{}
	for _, choice := range loginChoices {
		rows = append(rows, row{pick: true, line: uiLine{{choice[1], ""}}})
	}
	key, i := "", 0
	for key != "enter" {
		var err error
		if key, i, err = a.list(view{title: "add account", keys: "[enter] sign in  [esc] back", rows: rows}, i); err != nil {
			return nil, err
		}
		if key == "esc" || key == "q" {
			return nil, errCancelled
		}
	}
	service, mode, _ := strings.Cut(loginChoices[i][0], " ")
	before, _ := a.accounts(service)
	a.clear()
	a.out.Write([]byte("  " + a.renderLine(heading(loginChoices[i][1]), 120) + "\n\n"))
	unlock, err := a.lock("native")
	if err != nil {
		return nil, err
	}
	defer unlock()
	switch mode {
	case "repair":
		_, err = a.run("m365", []string{"cli", "app", "reconsent", "--output", "text"}, false, nil)
		return nil, err
	case "setup":
		if err = a.officeOrganization(); err != nil {
			return nil, err
		}
	}
	if err = a.login(service); err != nil {
		return nil, err
	}
	after, err := a.accounts(service)
	if err != nil {
		return nil, err
	}
	if len(after) == 0 {
		return nil, errors.New("sign in returned no account")
	}
	known := map[string]bool{}
	for _, r := range before {
		known[r.id()+r.Account] = true
	}
	added := []member{}
	for _, r := range after {
		if !known[r.id()+r.Account] {
			added = append(added, r.member)
		}
	}
	return added, nil
}

// RemoveLogin deletes a saved login after confirmation and removes it from
// every session. Azure signs out all subscriptions of the same user.
func (a *app) removeLogin(r account) error {
	if r.State == "Missing account" {
		return errors.New("this login is not connected; untick it instead")
	}
	if !ready(r.State) && r.State != "Sign in again" {
		return errors.New("cannot verify this login; sign in again first")
	}
	all, err := a.sessions()
	if err != nil {
		return err
	}
	match := func(m member) bool {
		return m.id() == r.id() || m.Service == "azure" && r.Service == "azure" && m.Account == r.Account
	}
	users := []string{}
	for _, s := range all {
		if len(without(s.Accounts, match)) != len(s.Accounts) {
			users = append(users, s.Name)
		}
	}
	name, _ := r.display()
	question := "delete login " + serviceName(r.Service) + " " + name + "?"
	if len(users) > 0 {
		question += " used by " + strings.Join(users, ", ") + "."
	}
	if !a.ask(question) {
		return errCancelled
	}
	unlock, err := a.lock("native")
	if err != nil {
		return err
	}
	defer unlock()
	if err = a.deleteLogin(r.member); err != nil {
		return err
	}
	for _, s := range all {
		err = a.updateSession(s.ID, func(current *session) error {
			current.Accounts = without(current.Accounts, match)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
