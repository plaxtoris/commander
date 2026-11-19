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

func (a *app) checkHealth(all []session) <-chan struct{} {
	done := make(chan struct{})
	members, seen := []member{}, map[string]bool{}
	for _, s := range all {
		for _, m := range s.Accounts {
			if !seen[m.id()] {
				members, seen[m.id()] = append(members, m), true
			}
		}
	}
	go func() {
		defer close(done)
		a.resolve(members)
	}()
	return done
}
