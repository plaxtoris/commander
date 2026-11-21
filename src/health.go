package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Health stores only account identifiers, display states and check times.
// It is advisory; commands still verify accounts before using them.
type accountHealth struct {
	State   string    `json:"state"`
	Checked time.Time `json:"checked"`
}

func (a *app) health() map[string]accountHealth {
	result := map[string]accountHealth{}
	path := filepath.Join(a.root, ".sessions", "health.json")
	if noSymlinks(a.root, path) != nil {
		return result
	}
	data, err := os.ReadFile(path)
	if err == nil {
		json.Unmarshal(data, &result)
	}
	if result == nil {
		result = map[string]accountHealth{}
	}
	return result
}

func (a *app) rememberHealth(rows []account) {
	a.rememberHealthAt(rows, time.Now().UTC())
}

func (a *app) rememberHealthAt(rows []account, checked time.Time) {
	if len(rows) == 0 {
		return
	}
	unlock, err := a.waitLock("health")
	if err != nil {
		return
	}
	defer unlock()
	states := a.health()
	for _, r := range rows {
		if !states[r.id()].Checked.After(checked) {
			states[r.id()] = accountHealth{State: r.State, Checked: checked}
		}
	}
	path := filepath.Join(a.root, ".sessions", "health.json")
	if noSymlinks(a.root, path) == nil {
		atomicWrite(path, encode(states), 0600)
	}
}

func attentionLabel(n int) string {
	if n == 1 {
		return "1 account needs attention"
	}
	return count(n, "account") + " need attention"
}

func healthIssues(s session, states map[string]accountHealth) []account {
	rows := []account{}
	for _, m := range s.Accounts {
		if h, ok := states[m.id()]; ok && !ready(h.State) {
			rows = append(rows, account{member: m, State: h.State})
		}
	}
	return rows
}

type accountIssue struct {
	account
	first    int
	sessions []string
}

func accountIssues(all []session, states map[string]accountHealth) []accountIssue {
	var issues []accountIssue
	index := map[string]int{}
	for n, s := range all {
		for _, r := range healthIssues(s, states) {
			id := r.id()
			i, exists := index[id]
			if !exists {
				i = len(issues)
				index[id] = i
				issues = append(issues, accountIssue{account: r, first: n})
			}
			issues[i].sessions = append(issues[i].sessions, s.Name)
		}
	}
	return issues
}

// Only background startup checks reuse recent successes. Explicit checks
// and commands that verify identity go straight to the provider.
func (a *app) checkHealth(all []session, recent map[string]accountHealth) <-chan struct{} {
	members, seen := []member{}, map[string]bool{}
	for _, s := range all {
		for _, m := range s.Accounts {
			id := m.id()
			h := recent[id]
			age := time.Since(h.Checked)
			if !seen[id] && !(ready(h.State) && age >= 0 && age < 2*time.Minute) {
				members, seen[id] = append(members, m), true
			}
		}
	}
	if len(members) == 0 {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.resolve(members)
	}()
	return done
}
