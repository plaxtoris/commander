package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
	"github.com/yeka/zip"
	"go.yaml.in/yaml/v3"
	_ "modernc.org/sqlite"
)

const maxArchive = 256 << 20
const archiveEntry = "commander-profile.json"
const azureClient = "04b07795-8ddb-461a-bbee-02f9e1bf7b46"

var errFormat = errors.New("unsupported or incomplete login data; sign in again before exporting")
var errConflict = errors.New("an imported login conflicts with a saved login; existing data was not changed")

type object = map[string]any

func obj(v any) object {
	m, _ := v.(map[string]any)
	if m == nil {
		return object{}
	}
	return m
}
func textValue(v any) string { s, _ := v.(string); return s }
func list(v any) []any {
	s, _ := v.([]any)
	if s == nil {
		return []any{}
	}
	return s
}
func encode(v any) []byte { data, _ := json.MarshalIndent(v, "", "  "); return append(data, '\n') }
func clone(v any) any     { var out any; json.Unmarshal(encode(v), &out); return out }
func contains(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

type profileArchive struct {
	Format   string            `json:"format"`
	Version  int               `json:"version"`
	Profile  selections        `json:"profile"`
	Logins   map[string]object `json:"logins"`
	Accounts []member          `json:"accounts,omitempty"`
	// Keep reading archives created before shared agent settings.
	Memory        *string       `json:"memory,omitempty"`
	CodingConfigs codingConfigs `json:"codingConfigs,omitempty"`
}

var sessionFile = regexp.MustCompile(`^\.sessions/[a-f0-9]{24}/session\.json$`)

type storage struct {
	root      string
	originals map[string][]byte
}

var nativeFiles = map[string]bool{
	".config/gh/hosts.yml": true, ".config/hcloud/cli.toml": true,
	".azure/azureProfile.json": true, ".azure/msal_token_cache.json": true,
	".cli-m365-all-connections.json": true, ".cli-m365-connection.json": true, ".cli-m365-msal-cache.json": true,
	".config/configstore/cli-m365-config.json": true, ".config/gcloud/credentials.db": true,
	".config/commander/dockerhub.json": true, ".config/commander/confluence.json": true,
}

func newStorage(root string) *storage { return &storage{root: root, originals: map[string][]byte{}} }
func (s *storage) read(name string) ([]byte, error) {
	if name != codingConfigFile && !nativeFiles[name] && !codingFile(name) {
		return nil, errors.New("path is outside native login storage")
	}
	path := filepath.Join(s.root, ".secrets", filepath.FromSlash(name))
	if err := noSymlinks(s.root, path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		s.originals[filepath.Join(".secrets", name)] = nil
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("could not access Commander storage")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArchive {
		return nil, errFormat
	}
	data, err := io.ReadAll(io.LimitReader(f, maxArchive+1))
	if err != nil || len(data) > maxArchive {
		return nil, errFormat
	}
	s.originals[filepath.Join(".secrets", name)] = data
	return data, nil
}
func (s *storage) json(name string, fallback any) (any, error) {
	data, err := s.read(name)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return clone(fallback), nil
	}
	var result any
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if json.Unmarshal(data, &result) != nil {
		return nil, errFormat
	}
	return result, nil
}

func (s *storage) state(service string, members ...member) (object, error) {
	if codingService(service) {
		return s.codingState(service, members)
	}
	if tokenService(service) {
		value, err := s.json(tokenFile(service), object{})
		if err != nil {
			return nil, err
		}
		state, ok := value.(map[string]any)
		if !ok {
			return nil, errFormat
		}
		return state, nil
	}
	switch service {
	case "github":
		data, err := s.read(".config/gh/hosts.yml")
		if err != nil {
			return nil, err
		}
		var state object
		if len(data) == 0 {
			return object{}, nil
		}
		if yaml.Unmarshal(data, &state) != nil {
			return nil, errFormat
		}
		normalized := obj(clone(state))
		return normalized, nil
	case "hetzner":
		data, err := s.read(".config/hcloud/cli.toml")
		if err != nil {
			return nil, err
		}
		var state object
		if len(data) == 0 {
			return object{}, nil
		}
		if toml.Unmarshal(data, &state) != nil {
			return nil, errFormat
		}
		return obj(clone(state)), nil
	case "azure":
		profile, err := s.json(".azure/azureProfile.json", object{"subscriptions": []any{}})
		if err != nil {
			return nil, err
		}
		cache, err := s.json(".azure/msal_token_cache.json", object{})
		return object{"profile": profile, "cache": cache}, err
	case "office":
		connections, err := s.json(".cli-m365-all-connections.json", []any{})
		if err != nil {
			return nil, err
		}
		current, err := s.json(".cli-m365-connection.json", object{})
		if err != nil {
			return nil, err
		}
		cache, err := s.json(".cli-m365-msal-cache.json", object{})
		if err != nil {
			return nil, err
		}
		if textValue(obj(current)["name"]) != "" {
			entries := []any{current}
			for _, row := range list(connections) {
				if obj(row)["name"] != obj(current)["name"] {
					entries = append(entries, row)
				}
			}
			connections = entries
		}
		return object{"connections": connections, "current": current, "cache": cache}, nil
	case "gcloud":
		name := ".config/gcloud/credentials.db"
		data, err := s.read(name)
		if err != nil {
			return nil, err
		}
		if data == nil {
			return object{}, nil
		}
		uri := (&url.URL{Scheme: "file", Path: filepath.Join(s.root, ".secrets", name), RawQuery: "mode=ro"}).String()
		db, err := sql.Open("sqlite", uri)
		if err != nil {
			return nil, errFormat
		}
		defer db.Close()
		var tables int
		if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil || tables != 1 {
			return nil, errFormat
		}
		rows, err := db.Query("SELECT account_id, value FROM credentials")
		if err != nil {
			return nil, errFormat
		}
		defer rows.Close()
		result := object{}
		for rows.Next() {
			var key string
			var value []byte
			if rows.Scan(&key, &value) != nil {
				return nil, errFormat
			}
			var record any
			if json.Unmarshal(value, &record) != nil {
				return nil, errFormat
			}
			result[key] = record
		}
		if rows.Err() != nil {
			return nil, errFormat
		}
		return result, nil
	}
	return nil, errFormat
}

func selectMSAL(cache object, identities, clients, localIDs, realms []string) (object, error) {
	accounts := object{}
	matched := map[string]bool{}
	for key, v := range obj(cache["Account"]) {
		r := obj(v)
		user := strings.ToLower(textValue(r["username"]))
		if contains(identities, user) && (localIDs == nil || contains(localIDs, textValue(r["local_account_id"]))) {
			accounts[key] = r
			if contains(realms, textValue(r["realm"])) {
				matched[user] = true
			}
		}
	}
	if realms != nil {
		for key, v := range accounts {
			r := obj(v)
			if matched[strings.ToLower(textValue(r["username"]))] && !contains(realms, textValue(r["realm"])) {
				delete(accounts, key)
			}
		}
	}
	homes := []string{}
	for _, identity := range identities {
		distinct := map[string]bool{}
		for _, v := range accounts {
			r := obj(v)
			if strings.EqualFold(textValue(r["username"]), identity) {
				home := textValue(r["home_account_id"])
				if home == "" {
					return nil, errFormat
				}
				distinct[home+"/"+textValue(r["environment"])] = true
				if !contains(homes, home) {
					homes = append(homes, home)
				}
			}
		}
		if len(distinct) != 1 {
			return nil, errors.New("Microsoft cache does not uniquely match the selected account and tenant")
		}
	}
	result := object{"Account": accounts}
	refresh := map[string]bool{}
	for _, category := range []string{"AccessToken", "RefreshToken", "IdToken", "AppMetadata"} {
		selected := object{}
		for key, v := range obj(cache[category]) {
			r := obj(v)
			if contains(clients, textValue(r["client_id"])) && (category == "AppMetadata" || contains(homes, textValue(r["home_account_id"]))) && (realms == nil || category == "AppMetadata" || category == "RefreshToken" || contains(realms, textValue(r["realm"]))) {
				selected[key] = r
				if category == "RefreshToken" {
					refresh[textValue(r["home_account_id"])] = true
				}
			}
		}
		result[category] = selected
	}
	for _, home := range homes {
		if !refresh[home] {
			return nil, errors.New("no saved refresh token for the selected Microsoft account; sign in again")
		}
	}
	return result, nil
}

func selectLogins(service string, state object, members []member) (object, error) {
	if codingService(service) {
		out := object{}
		for _, m := range members {
			key := m.selector()
			if !codingLabel.MatchString(key) || key != m.Account || !validCodingRecord(service, state[key]) {
				return nil, errFormat
			}
			out[key] = state[key]
		}
		return out, nil
	}
	if tokenService(service) {
		selected := object{}
		for _, m := range members {
			c, err := decodeTokenLogin(service, m.selector(), state[m.selector()])
			if err != nil || c.member(service).Account != m.Account || c.member(service).Context != m.Context {
				return nil, errFormat
			}
			selected[m.selector()] = obj(clone(c))
		}
		return selected, nil
	}
	if len(members) == 0 {
		return nil, errFormat
	}
	switch service {
	case "github":
		selected := object{}
		for _, m := range members {
			host, user := m.github()
			if host == "" || m.Account != user+" @ "+host {
				return nil, errFormat
			}
			entry := obj(state[host])
			saved := obj(obj(entry["users"])[user])
			credential := object{}
			if refresh, ok := saved["refreshable_oauth_token"]; ok {
				r := obj(refresh)
				if textValue(r["access_token"]) == "" || textValue(r["refresh_token"]) == "" {
					return nil, errFormat
				}
				kept := object{}
				for _, field := range []string{"access_token", "refresh_token", "expires_in", "expires_at", "refresh_token_expires_in", "refresh_token_expires_at"} {
					if value, ok := r[field]; ok {
						kept[field] = value
					}
				}
				credential["refreshable_oauth_token"] = kept
			} else {
				token := textValue(saved["oauth_token"])
				if token == "" && entry["user"] == user {
					token = textValue(entry["oauth_token"])
				}
				if token == "" {
					return nil, errors.New("GitHub login is missing or stored outside Commander; sign in again")
				}
				credential["oauth_token"] = token
			}
			target, ok := selected[host]
			if !ok {
				protocol := textValue(entry["git_protocol"])
				if protocol == "" {
					protocol = "https"
				}
				target = object{"users": object{}, "git_protocol": protocol, "user": user}
				if token, ok := credential["oauth_token"]; ok {
					obj(target)["oauth_token"] = token
				}
				selected[host] = target
			}
			obj(obj(target)["users"])[user] = credential
		}
		return selected, nil
	case "hetzner":
		selected := []any{}
		for _, m := range members {
			if m.Account != m.selector() {
				return nil, errFormat
			}
			found := false
			for _, v := range list(state["contexts"]) {
				r := obj(v)
				if r["name"] == m.selector() {
					if textValue(r["token"]) == "" {
						return nil, errFormat
					}
					selected = append(selected, object{"name": m.selector(), "token": r["token"]})
					found = true
					break
				}
			}
			if !found {
				return nil, errFormat
			}
		}
		return object{"active_context": obj(selected[0])["name"], "contexts": selected}, nil
	case "azure":
		selected := []any{}
		identities, realms := []string{}, []string{}
		for _, m := range members {
			found := false
			for _, v := range list(obj(state["profile"])["subscriptions"]) {
				r := obj(v)
				if r["id"] != m.selector() {
					continue
				}
				if obj(r["user"])["name"] != m.Account || obj(r["user"])["type"] != "user" {
					return nil, errFormat
				}
				r = obj(clone(r))
				r["isDefault"] = len(selected) == 0
				selected = append(selected, r)
				identities = append(identities, strings.ToLower(m.Account))
				realms = append(realms, textValue(r["tenantId"]))
				found = true
				break
			}
			if !found {
				return nil, errFormat
			}
		}
		cache, err := selectMSAL(obj(state["cache"]), identities, []string{azureClient}, nil, realms)
		return object{"profile": object{"subscriptions": selected}, "cache": cache}, err
	case "office":
		selected := []any{}
		cache := object{}
		for _, m := range members {
			found := false
			for _, v := range list(state["connections"]) {
				r := obj(v)
				if r["name"] != m.selector() {
					continue
				}
				if r["identityName"] != m.Account || !contains([]string{"deviceCode", "browser"}, textValue(r["authType"])) {
					return nil, errFormat
				}
				r = obj(clone(r))
				r["active"] = len(selected) == 0
				part, err := selectMSAL(obj(state["cache"]), []string{strings.ToLower(m.Account)}, []string{textValue(r["appId"])}, []string{textValue(r["identityId"])}, nil)
				if err != nil {
					return nil, err
				}
				cache, err = mergeCache(cache, part)
				if err != nil {
					return nil, err
				}
				selected = append(selected, r)
				found = true
				break
			}
			if !found {
				return nil, errFormat
			}
		}
		return object{"connections": selected, "current": selected[0], "cache": cache}, nil
	case "gcloud":
		selected := object{}
		for _, m := range members {
			r := obj(state[m.selector()])
			if m.Account != m.selector() || r["type"] != "authorized_user" || textValue(r["refresh_token"]) == "" {
				return nil, errFormat
			}
			selected[m.selector()] = r
		}
		return selected, nil
	}
	return nil, errFormat
}

func mergeMap(existing, incoming object) (object, error) {
	result := obj(clone(existing))
	for key, value := range incoming {
		if previous, ok := result[key]; ok && !reflect.DeepEqual(previous, value) {
			return nil, errConflict
		}
		result[key] = value
	}
	return result, nil
}
func mergeCache(existing, incoming object) (object, error) {
	result := obj(clone(existing))
	for category, value := range incoming {
		merged, err := mergeMap(obj(existing[category]), obj(value))
		if err != nil {
			return nil, err
		}
		result[category] = merged
	}
	return result, nil
}

// Credential state is already scoped or merged by the caller.
func encodeLogins(service string, state object) (map[string][]byte, error) {
	files := map[string][]byte{}
	if codingService(service) {
		for key, v := range state {
			if !codingLabel.MatchString(key) || !validCodingRecord(service, v) {
				return nil, errFormat
			}
			files[filepath.Join("coding", service, key, credentialFiles[service])] = encode(v)
		}
		return files, nil
	}
	if tokenService(service) {
		files[tokenFile(service)] = encode(state)
		return files, nil
	}
	switch service {
	case "github":
		files[".config/gh/hosts.yml"] = encode(state)
	case "hetzner":
		rows, err := uniqueLoginRows(list(state["contexts"]), "name")
		if err != nil {
			return nil, err
		}
		data, err := toml.Marshal(object{"active_context": textValue(state["active_context"]), "contexts": rows})
		if err != nil {
			return nil, errFormat
		}
		files[".config/hcloud/cli.toml"] = data
	case "azure":
		profile := obj(state["profile"])
		rows, err := uniqueLoginRows(list(profile["subscriptions"]), "id", "isDefault")
		if err != nil {
			return nil, err
		}
		profile = obj(clone(profile))
		profile["subscriptions"] = rows
		files[".azure/azureProfile.json"] = encode(profile)
		files[".azure/msal_token_cache.json"] = encode(state["cache"])
	case "office":
		rows, err := uniqueLoginRows(list(state["connections"]), "name", "active")
		if err != nil {
			return nil, err
		}
		current := obj(state["current"])
		files[".cli-m365-all-connections.json"] = encode(rows)
		files[".cli-m365-connection.json"] = encode(current)
		files[".cli-m365-msal-cache.json"] = encode(state["cache"])
		files[".config/configstore/cli-m365-config.json"] = encode(object{"clientId": current["appId"], "tenantId": current["tenant"], "authType": "deviceCode", "autoOpenLinksInBrowser": false, "copyDeviceCodeToClipboard": false})
	case "gcloud":
		data, err := encodeCloudLogins(state)
		if err != nil {
			return nil, err
		}
		files[".config/gcloud/credentials.db"] = data
	default:
		return nil, errFormat
	}
	return files, nil
}

func uniqueLoginRows(rows []any, key string, flags ...string) ([]any, error) {
	index := map[string]object{}
	result := make([]any, 0, len(rows))
	for _, row := range rows {
		normalized := obj(clone(row))
		for _, flag := range flags {
			delete(normalized, flag)
		}
		id := textValue(obj(row)[key])
		if previous, ok := index[id]; ok {
			if !reflect.DeepEqual(previous, normalized) {
				return nil, errConflict
			}
			continue
		}
		result = append(result, row)
		index[id] = normalized
	}
	return result, nil
}

func encodeCloudLogins(records object) ([]byte, error) {
	file, err := os.CreateTemp("", "commander-database-*")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer os.Remove(path)
	if err = file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errFormat
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, errFormat
	}
	defer tx.Rollback()
	if _, err = tx.Exec("CREATE TABLE credentials (account_id TEXT PRIMARY KEY, value BLOB)"); err != nil {
		return nil, errFormat
	}
	for key, value := range records {
		if _, err = tx.Exec("INSERT INTO credentials VALUES (?, ?)", key, encode(value)); err != nil {
			return nil, errFormat
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, errFormat
	}
	if err = db.Close(); err != nil {
		return nil, errFormat
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errFormat
	}
	return data, nil
}

func (s *storage) publish(files map[string][]byte, retired ...string) error {
	if len(files) == 0 && len(retired) == 0 {
		return nil
	}
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		path := filepath.ToSlash(name)
		login, private := strings.CutPrefix(path, ".secrets/")
		if path != "MEMORY.md" && !sessionFile.MatchString(path) && !(private && (login == codingConfigFile || nativeFiles[login] || codingFile(login))) {
			return errFormat
		}
		if err := noSymlinks(s.root, filepath.Join(s.root, name)); err != nil {
			return err
		}
	}
	stage, err := os.MkdirTemp(s.root, ".import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for i, name := range paths {
		if err = os.WriteFile(filepath.Join(stage, fmt.Sprint(i)), files[name], 0600); err != nil {
			return err
		}
	}
	for name, before := range s.originals {
		now, err := os.ReadFile(filepath.Join(s.root, name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if !bytes.Equal(before, now) || (before == nil) != (now == nil) {
			return errors.New("login files changed during import; retry after native commands finish")
		}
	}
	changed := []string{}
	moved := []string{}
	rollback := func() {
		for i := len(changed) - 1; i >= 0; i-- {
			name := changed[i]
			path := filepath.Join(s.root, name)
			if before := s.originals[name]; before != nil {
				atomicWrite(path, before, 0600)
			} else {
				os.Remove(path)
				if sessionFile.MatchString(filepath.ToSlash(name)) {
					os.Remove(filepath.Dir(path))
				}
			}
		}
		for _, dir := range moved {
			os.Rename(filepath.Join(stage, filepath.Base(dir)), dir)
		}
	}
	for _, id := range retired {
		if !sessionID.MatchString(id) {
			rollback()
			return errFormat
		}
		dir := filepath.Join(s.root, ".sessions", id)
		if err = noSymlinks(s.root, dir); err != nil {
			rollback()
			return err
		}
		if err = os.Rename(dir, filepath.Join(stage, id)); err != nil {
			rollback()
			return err
		}
		moved = append(moved, dir)
	}
	for i, name := range paths {
		path := filepath.Join(s.root, name)
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			rollback()
			return err
		}
		if err = os.Rename(filepath.Join(stage, fmt.Sprint(i)), path); err != nil {
			rollback()
			return err
		}
		changed = append(changed, name)
	}
	return nil
}

// WriteArchive never overwrites an existing file unless replace is set; then
// the old archive stays intact until the new one is complete.
func writeArchive(path string, payload profileArchive, password string, replace bool) error {
	if _, err := os.Lstat(path); !replace && !os.IsNotExist(err) {
		return errors.New("archive already exists; choose another filename")
	}
	data := encode(payload)
	if len(data) > maxArchive {
		return errors.New("profile exceeds archive limits")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".export-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	z := zip.NewWriter(f)
	entry, err := z.Encrypt(archiveEntry, password, zip.AES256Encryption)
	if err == nil {
		_, err = entry.Write(data)
	}
	if err == nil {
		err = z.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if replace {
		return os.Rename(f.Name(), path)
	}
	return os.Link(f.Name(), path)
}

func readArchive(path, password string) (profileArchive, error) {
	var payload profileArchive
	invalid := errors.New("cannot open profile archive; check its password and integrity")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Size() > maxArchive+(1<<20) {
		return payload, invalid
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		return payload, invalid
	}
	defer z.Close()
	if len(z.File) != 1 {
		return payload, invalid
	}
	f := z.File[0]
	if f.Name != archiveEntry || !f.IsEncrypted() || f.UncompressedSize64 > maxArchive {
		return payload, invalid
	}
	aes := false
	extra := f.Extra
	for len(extra) >= 4 {
		kind := binary.LittleEndian.Uint16(extra)
		n := int(binary.LittleEndian.Uint16(extra[2:]))
		if n > len(extra)-4 {
			return payload, invalid
		}
		value := extra[4 : 4+n]
		if kind == 0x9901 {
			aes = n == 7 && string(value[2:4]) == "AE" && value[4] == 3
		}
		extra = extra[4+n:]
	}
	if !aes {
		return payload, invalid
	}
	f.SetPassword(password)
	r, err := f.Open()
	if err != nil {
		return payload, invalid
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxArchive+1))
	if err != nil || len(data) > maxArchive || uint64(len(data)) != f.UncompressedSize64 {
		return payload, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil {
		return payload, invalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return payload, invalid
	}
	return payload, nil
}
