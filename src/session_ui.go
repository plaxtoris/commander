package main

import (
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
)

const homeKeys = "[enter] start  [e] edit  [d] delete  [c] check  [x] export  [i] import  [q] quit"
const editKeys = "[space] toggle  [r] rename  [esc] back"

type menuAction struct{ key, label string }

func (a *app) actions(title string, actions []menuAction) (string, error) {
	labels := make([]string, len(actions))
	for i, action := range actions {
		labels[i] = action.label
	}
	i, err := a.choose(title, labels, 0)
	if err != nil {
		return "", err
	}
	return actions[i].key, nil
}

func (a *app) menu() error {
	title := ""
	if a.session != nil {
		title = "session " + a.session.Name
	}
	admin := a.admin()
	notice, tone, focus, at := "", "", admin.lastSession(), 0
	var checking <-chan struct{}
	check := true
	var restore func()
	defer func() {
		if restore != nil {
			restore()
		}
	}()
	for {
		all, err := admin.sessions()
		if err != nil {
			return err
		}
		states := admin.health()
		if check {
			checking, check = admin.checkHealth(all, states), false
		}
		names := 0
		for _, s := range all {
			names = max(names, min(24, textWidth(s.Name)))
		}
		// One calm line per session; the selected one lists its accounts below.
		rows := []row{}
		for i, s := range all {
			used := s.summary()
			r := row{pick: true, line: uiLine{{pad(s.Name, names), "1"}, {"   " + used, "2"}}}
			issues := healthIssues(s, states)
			if len(issues) > 0 {
				r.line = append(r.line, uiSpan{"  !", "33"})
			}
			r.detail = func(width int) []uiLine {
				groups := s.groups()
				scope := groups[:0]
				for _, g := range groups {
					switch g.label {
					case "github", "repositories":
						scope = append(scope, g)
					case "accounts", "codex", "claude", serviceName("litellm"):
						// Already shown on the session row.
					default:
						if len(groups) <= 3 {
							scope = append(scope, g)
						}
					}
				}
				return groupLines(scope, width)
			}
			if s.ID == focus {
				at = i
			}
			rows = append(rows, r)
		}
		rows = append(rows, row{pick: true, line: uiLine{{"+ new session", "36"}}, keys: "[enter] create  [c] check  [x] export  [i] import  [q] quit"})
		issues := accountIssues(all, states)
		keys := homeKeys
		if len(issues) > 0 {
			keys = "[l] account issues  " + keys
			rows[len(all)].keys = "[l] account issues  " + rows[len(all)].keys
			if notice == "" {
				notice, tone = attentionLabel(len(issues)), "33"
			}
		}
		info := count(len(all), "session")
		if checking != nil {
			info += " · checking accounts…"
		}
		// Keep input raw across automatic redraws, including keys typed while
		// the refreshed session rows are being built. Actions restore it below.
		if restore == nil {
			restore, err = admin.raw()
			if err != nil {
				return err
			}
		}
		key, i, err := admin.list(view{title: title, info: info, keys: keys, notice: notice, tone: tone, rows: rows, refresh: checking}, at)
		if err != nil {
			return err
		}
		if key != "refresh" {
			restore()
			restore = nil
		}
		notice, tone, focus, at = "", "32", "", i
		picked := len(all) > 0 && i < len(all)
		var s session
		if picked {
			s = all[i]
		}
		switch {
		case key == "refresh":
			checking = nil
		case key == "q" || key == "esc":
			admin.clear()
			return nil
		case key == "i":
			notice, err = admin.importArchive("")
		case key == "x":
			notice, err = admin.chooseExport()
		case key == "c":
			if checking == nil {
				checking = admin.checkHealth(all, nil)
			}
		case key == "l" && len(issues) > 0:
			notice, err = admin.reviewAccountIssues(all, issues)
		case key == "n" || key == "enter" && i == len(all):
			s, err = admin.createSession()
			if err == nil {
				notice, focus = s.Name+" created", s.ID
				for _, m := range s.Accounts {
					if codingService(m.Service) {
						err = admin.code(s)
						break
					}
				}
			}
		case !picked:
		case key == "enter":
			err = admin.code(s)
			focus = s.ID
		case key == "e" || key == "l":
			var selected string
			if issues := healthIssues(s, states); len(issues) > 0 {
				selected = issues[0].id()
			}
			focus = s.ID
			_, _, err = admin.editSelection(s.Name, &s, selected)
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

func (a *app) reviewAccountIssues(all []session, issues []accountIssue) (string, error) {
	rows := make([]row, len(issues))
	for i, issue := range issues {
		keys := "[enter] check again  [esc] back"
		if issue.canRepair() {
			keys = "[enter] sign in again  [c] check again  [esc] back"
		} else if issue.State == "Missing account" {
			keys = "[enter] edit accounts  [c] check again  [esc] back"
		}
		status, tone := statusLabel(issue.State)
		line := append(uiLine{{serviceName(issue.Service) + "  ", "2"}}, issue.item()...)
		rows[i] = row{pick: true, line: append(line, uiSpan{"  " + status, tone}),
			keys: keys,
			detail: func(width int) []uiLine {
				return uiLine{{"sessions  " + strings.Join(issue.sessions, ", "), "2"}}.wrap(width)
			}}
	}
	at := 0
	for {
		key, i, err := a.list(view{title: "account issues", rows: rows}, at)
		if err != nil || key == "esc" || key == "q" {
			return "", err
		}
		at = i
		if key != "enter" && key != "c" {
			continue
		}
		issue := issues[i]
		current := a.resolve([]member{issue.member})[0]
		name, _ := issue.display()
		if ready(current.State) {
			return name + " connected", nil
		}
		if key == "enter" && current.State == issue.State {
			if issue.canRepair() {
				if err := a.reconnect(issue.member); err != nil {
					return "", err
				}
				return name + " connected", nil
			}
			if issue.State == "Missing account" {
				target := all[issue.first]
				_, _, err := a.editSelection(target.Name, &target, issue.id())
				a.resolve([]member{issue.member})
				return "", err
			}
		}
		return "", errors.New(name + " · " + strings.ToLower(current.State))
	}
}

func (a *app) createSession() (session, error) {
	name, err := a.prompt("name", "", nil)
	if err != nil {
		return session{}, err
	}
	if err = a.checkName(name, ""); err != nil {
		return session{}, err
	}
	accounts, repos, err := a.editSelection(name, nil)
	if err != nil {
		return session{}, err
	}
	return a.newSession(name, accounts, repos)
}

func (a *app) loading(title string) {
	if a.terminal() {
		a.clear()
		a.out.Write([]byte("  " + a.renderLine(heading(title), 120) + "\r\n\r\n  " + a.style("loading accounts…", "2") + "\r\n"))
	}
}

// What an edit screen row stands for.
type target struct {
	login  int
	scope  string
	add    bool
	repo   bool
	expand bool
}

// Editing an existing session saves each change before redrawing. A new
// session collects its selections until the user creates it.
func (a *app) editSelection(title string, existing *session, focus ...string) ([]member, []string, error) {
	a = a.admin()
	creating := existing == nil
	var accounts []member
	var repos []string
	if existing != nil {
		accounts, repos = existing.Accounts, existing.Repositories
	}
	chosen, ticked := map[string]bool{}, map[string]bool{}
	routes := map[string]string{}
	for _, m := range accounts {
		chosen[m.id()] = true
		for _, repo := range m.Repos {
			routes[strings.ToLower(repo)] = m.id()
		}
	}
	for _, r := range repos {
		ticked[strings.ToLower(r)] = true
	}
	owners := map[string][]string{}
	var logins []account
	var failed map[string]string
	notice, tone, at, reload := "", "", 0, true
	showScope := !creating
selection:
	for {
		if reload {
			a.loading(title)
			if showScope {
				logins, failed = a.logins()
			} else {
				logins, failed = a.logins("codex", "claude", "litellm")
			}
			known := map[string]bool{}
			for _, r := range logins {
				known[r.id()] = true
			}
			for _, m := range accounts {
				if !known[m.id()] {
					state := "Missing account"
					if failed[m.Service] != "" {
						state = failed[m.Service]
					}
					logins = append(logins, account{member: m, State: state})
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
		githubSelected := func() []member {
			var members []member
			for _, r := range logins {
				if r.Service == "github" && chosen[r.id()] {
					members = append(members, r.member)
				}
			}
			return members
		}
		gitLogins := githubSelected()
		route := func(scope string) string {
			if id := routes[strings.ToLower(scope)]; chosen[id] {
				return id
			}
			if len(gitLogins) == 1 {
				return gitLogins[0].id()
			}
			single := ""
			owner, _, _ := strings.Cut(strings.ToLower(scope), "/")
			if id := routes[owner+"/*"]; ticked[owner+"/*"] && chosen[id] {
				return id
			}
			for _, m := range gitLogins {
				for _, candidate := range owners[m.id()] {
					if strings.EqualFold(candidate, owner+"/*") {
						if single != "" && single != m.id() {
							return ""
						}
						single = m.id()
					}
				}
			}
			return single
		}
		chooseRoute := func(scope string) error {
			names := make([]string, len(gitLogins))
			current, id := 0, route(scope)
			for i, m := range gitLogins {
				names[i] = m.ref()
				if id == m.id() {
					current = i
				}
			}
			pick, err := a.choose("github · "+scope, names, current)
			if err == nil {
				routes[strings.ToLower(scope)] = gitLogins[pick].id()
			}
			return err
		}
		keys := editKeys
		quickStart := false
		if creating {
			quickStart = true
			for _, r := range logins {
				if chosen[r.id()] && codingService(r.Service) {
					quickStart = false
					break
				}
			}
			keys = "[enter] start  [space] select  [esc] cancel"
			if quickStart {
				keys = "[enter] create  [space] select  [esc] cancel"
			}
		}
		back := "[esc] back"
		if creating {
			back = "[esc] cancel"
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
				rowKeys := keys
				if quickStart && codingService(r.Service) && ready(r.State) {
					rowKeys = "[enter] start  [space] select  [esc] cancel"
				}
				if ready(r.State) || r.State == "Sign in again" {
					rowKeys = "[d] delete login  " + rowKeys
				}
				rendered := row{indent: 2, pick: true, line: line, keys: rowKeys}
				if r.canReconnect() {
					rendered.keys = "[l] sign in again  " + rendered.keys
				}
				if len(focus) > 0 && focus[0] == r.id() {
					at, focus = len(rows), nil
				}
				rows, targets = append(rows, rendered), append(targets, target{login: i})
			}
			if failed[service] != "" {
				if first {
					header(serviceName(service))
				}
				status, color := statusLabel(failed[service])
				rows, targets = append(rows, row{indent: 4, line: uiLine{{status, color}}}), append(targets, target{login: -1})
			}
			if service == "github" && (len(scopes) > 0 || len(gitLogins) > 0) {
				header("repositories")
				for _, scope := range scopes {
					line := uiLine{mark(ticked[strings.ToLower(scope)]), {" " + scope, ""}}
					r := row{indent: 2, pick: true, line: line}
					if len(gitLogins) > 1 {
						label := "choose login"
						for _, m := range gitLogins {
							if route(scope) == m.id() {
								label = m.ref()
								break
							}
						}
						r.line = append(r.line, uiSpan{"  · " + label, "2"})
						r.keys = "[g] github login  " + keys
					}
					rows, targets = append(rows, r), append(targets, target{login: -1, scope: scope})
				}
				rows, targets = append(rows, row{indent: 2, pick: true, line: uiLine{{"+ add repo", "36"}}, keys: "[enter] add repo  " + back}), append(targets, target{login: -1, repo: true})
			}
		}
		if len(logins) == 0 {
			label := "no accounts yet"
			if !showScope {
				label = "no agent accounts yet"
			}
			rows, targets = append(rows, row{line: uiLine{{label, "2"}}}), append(targets, target{login: -1})
		}
		addLabel := "add account"
		if !showScope {
			addLabel = "add agent"
		}
		rows, targets = append(rows, row{pick: true, line: uiLine{{"+ " + addLabel, "36"}}, keys: "[enter] " + addLabel + "  " + back}), append(targets, target{login: -1, add: true})
		if !showScope {
			rows = append(rows, row{pick: true, line: uiLine{{"accounts & repos", ""}}, keys: "[enter] choose scope  [esc] cancel"})
			targets = append(targets, target{login: -1, expand: true})
		}
		issues := 0
		for _, r := range logins {
			if !ready(r.State) {
				issues++
			}
		}
		info := ""
		if issues > 0 {
			info = "! " + attentionLabel(issues)
		}
		key, i, err := a.list(view{title: title, info: info, keys: keys, notice: notice, tone: tone, rows: rows}, at)
		if err != nil {
			return nil, nil, err
		}
		notice, tone, at = "", "", i
		t := targets[i]
		if key == "enter" && t.expand {
			showScope, reload = true, true
			continue
		}
		if key == "enter" && t.repo {
			key = "add-repo"
		}
		var oldChosen, oldTicked map[string]bool
		var oldRoutes map[string]string
		oldRepos := repos
		if existing != nil {
			oldChosen, oldTicked, oldRoutes = maps.Clone(chosen), maps.Clone(ticked), maps.Clone(routes)
		}
		changed := false
		switch {
		case key == "esc" || key == "q":
			if creating {
				return nil, nil, errCancelled
			}
			return nil, nil, nil
		case key == "r" && existing != nil:
			name, err := a.prompt("rename "+title, title, nil)
			if err == nil && name != title {
				if err = a.renameSession(*existing, name); err == nil {
					existing.Name, title = name, name
					notice, tone = "renamed to "+name, "32"
				}
			}
			if err != nil && !errors.Is(err, errCancelled) {
				notice = err.Error()
			}
		case key == "l" && t.login >= 0 && logins[t.login].canReconnect():
			r := logins[t.login]
			err = a.reconnect(r.member)
			if err == nil {
				name, _ := r.display()
				notice, tone = name+" connected", "32"
			} else if !errors.Is(err, errCancelled) {
				notice = err.Error()
			}
			focus, reload, owners = []string{r.id()}, true, map[string][]string{}
		case key == "a" || key == "enter" && t.add:
			added, err := a.addLogin(!showScope)
			if err != nil && !errors.Is(err, errCancelled) {
				notice = err.Error()
			}
			for _, m := range added {
				addedLogin := account{member: m, State: "Ready"}
				if i := slices.IndexFunc(logins, func(r account) bool { return r.id() == m.id() }); i >= 0 {
					logins[i] = addedLogin
				} else {
					logins = append(logins, addedLogin)
				}
				chosen[m.id()] = true
				focus = []string{m.id()}
				if !codingService(m.Service) {
					showScope = true
				}
			}
			reload = len(added) > 0 || err == nil
			changed = len(added) > 0
		case key == "enter" && creating:
			if quickStart && t.login >= 0 && codingService(logins[t.login].Service) && ready(logins[t.login].State) {
				chosen[logins[t.login].id()] = true
			}
		case key == "add-repo":
			scope, e := a.prompt("repo or org/*", "", nil)
			if e == nil {
				if validRepositoryScope(scope) {
					if len(gitLogins) > 1 && route(scope) == "" && chooseRoute(scope) != nil {
						continue
					}
					repos = append(repos, scope)
					ticked[strings.ToLower(scope)] = true
					scopes = append(scopes, scope)
					changed = true
				} else {
					notice = "use owner/repo or owner/*"
				}
			}
		case key == "g" && t.scope != "" && len(gitLogins) > 1:
			if chooseRoute(t.scope) == nil {
				ticked[strings.ToLower(t.scope)] = true
				changed = true
			}
		case key == "space" && t.login >= 0:
			id := logins[t.login].id()
			chosen[id] = !chosen[id]
			changed = true
		case key == "space" && t.scope != "":
			k := strings.ToLower(t.scope)
			if !ticked[k] && len(gitLogins) > 1 && route(t.scope) == "" && chooseRoute(t.scope) != nil {
				continue
			}
			ticked[k] = !ticked[k]
			changed = true
		case key == "d" && t.login >= 0:
			r := logins[t.login]
			err = a.removeLogin(r)
			if err == nil {
				for _, login := range logins {
					if loginMatchesRemoval(login.member, r.member) {
						delete(chosen, login.id())
					}
				}
				accounts = without(accounts, func(m member) bool { return loginMatchesRemoval(m, r.member) })
				if existing != nil {
					existing.Accounts = accounts
				}
				notice, tone, reload = "login deleted", "32", true
			} else if !errors.Is(err, errCancelled) {
				notice = err.Error()
			}
		}
		finish := creating && key == "enter" && !t.add
		if !finish && (creating || !changed) {
			continue
		}
		gitLogins = githubSelected()
		selected, scoped := []member{}, []string{}
		for _, r := range logins {
			if chosen[r.id()] {
				selected = append(selected, r.member)
			}
		}
		for _, scope := range scopes {
			if !ticked[strings.ToLower(scope)] || contains(scoped, scope) {
				continue
			}
			if len(gitLogins) > 1 && route(scope) == "" {
				if err := chooseRoute(scope); err != nil {
					if !creating {
						chosen, ticked, routes, repos = oldChosen, oldTicked, oldRoutes, oldRepos
					}
					if !errors.Is(err, errCancelled) {
						notice = err.Error()
					}
					continue selection
				}
			}
			scoped = append(scoped, scope)
		}
		for i, m := range selected {
			if m.Service == "github" {
				selected[i].Repos = nil
				for _, scope := range scoped {
					if route(scope) == m.id() && len(gitLogins) > 1 {
						selected[i].Repos = append(selected[i].Repos, scope)
					}
				}
			}
		}
		if finish {
			return selected, scoped, nil
		}
		if err := a.saveSelection(*existing, selected, scoped); err != nil {
			chosen, ticked, routes, repos = oldChosen, oldTicked, oldRoutes, oldRepos
			notice = err.Error()
			continue
		}
		existing.Accounts, existing.Repositories = selected, scoped
		accounts = selected
		notice, tone = "saved", "32"
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
func (a *app) addLogin(agentsOnly bool) ([]member, error) {
	var choices []menuAction
	for _, service := range services {
		if !agentsOnly || codingService(service) {
			choices = append(choices, menuAction{service, serviceName(service)})
		}
	}
	title := "add agent"
	if !agentsOnly {
		title = "add account"
		choices = append(choices, menuAction{"office setup", "microsoft 365 · new organization"}, menuAction{"office repair", "microsoft 365 · repair permissions"})
	}
	action, err := a.actions(title, choices)
	if err != nil {
		return nil, err
	}
	service, mode, _ := strings.Cut(action, " ")
	a.clear()
	a.out.Write([]byte("  " + a.renderLine(heading(serviceName(service)), 120) + "\n\n"))
	unlock, err := a.lock("native")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if codingService(service) {
		m, e := a.connectCoding(service, "")
		if e != nil {
			return nil, e
		}
		return []member{m}, nil
	}
	before, _ := a.queryAccounts(service)
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
	// The native lock already excludes status checks and account runtimes.
	after, err := a.queryAccounts(service)
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

func (m member) canReconnect() bool {
	return m.Service == "github" || codingService(m.Service) || tokenService(m.Service)
}

func (r account) canRepair() bool {
	return r.canReconnect() && (r.State == "Sign in again" || r.State == "Missing account")
}

func (a *app) reconnect(m member) error {
	a.clear()
	if m.Service == "github" {
		return a.reconnectGitHub(m)
	}
	unlock, err := a.lock("native")
	if err != nil {
		return err
	}
	defer unlock()
	if tokenService(m.Service) {
		err = a.loginToken(m.Service, &m)
	} else {
		_, err = a.connectCoding(m.Service, m.Account)
	}
	if err == nil {
		a.rememberHealth([]account{{member: m, State: "Ready"}})
	}
	return err
}

func loginMatchesRemoval(m, removed member) bool {
	return m.id() == removed.id() || m.Service == "azure" && removed.Service == "azure" && m.Account == removed.Account
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
		return loginMatchesRemoval(m, r.member)
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
