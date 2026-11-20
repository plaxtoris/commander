package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type toolRelease struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256,omitempty"`
}

var stableVersion = regexp.MustCompile(`^[0-9]{1,7}\.[0-9]{1,7}\.[0-9]{1,7}$`)
var checksum = regexp.MustCompile(`^[a-f0-9]{64}$`)

func newerTool(next, old string) bool {
	if !stableVersion.MatchString(next) || !stableVersion.MatchString(old) {
		return false
	}
	a, b := strings.Split(next, "."), strings.Split(old, ".")
	for i := range a {
		x, _ := strconv.Atoi(a[i])
		y, _ := strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return false
}
func (c catalog) copy() catalog {
	return catalog{Packages: maps.Clone(c.Packages), Archives: maps.Clone(c.Archives)}
}
func (c catalog) apply(name string, r toolRelease) {
	if old, ok := c.Archives[name]; ok {
		if newerTool(r.Version, old.Version) && checksum.MatchString(r.SHA256) {
			if name == "gcloud" {
				// Google publishes the SHA256 of its rolling archive. The
				// versioned gzip has a different header and a different hash.
				old.URL = strings.Replace(old.URL, "-"+old.Version+"-", "-", 1)
			} else {
				old.URL = strings.ReplaceAll(old.URL, old.Version, r.Version)
			}
			old.Version, old.SHA256 = r.Version, r.SHA256
			c.Archives[name] = old
		}
		return
	}
	if newerTool(r.Version, c.Packages[name]) {
		c.Packages[name] = r.Version
	}
}

// A successful atomic installation is the source of truth, including offline.
// Subcommands use the installed release even before another menu is opened.
func (a *app) installedCatalog(base catalog) catalog {
	c := base.copy()
	for name := range c.Archives {
		var r toolRelease
		b, _ := os.ReadFile(filepath.Join(a.packageDir(name), ".commander-release"))
		if json.Unmarshal(b, &r) == nil {
			c.apply(name, r)
		}
	}
	for name := range packageEntries {
		b, _ := os.ReadFile(filepath.Join(a.packageDir(name), ".commander-version"))
		v := strings.Fields(string(b))
		if len(v) > 0 {
			c.apply(name, toolRelease{Version: v[0]})
		}
	}
	if p, err := os.ReadDir(a.packageDir("python")); err == nil {
		for _, d := range p {
			if !d.IsDir() {
				continue
			}
			v := strings.Split(d.Name(), "-")
			if len(v) >= 3 && v[0] == "cpython" && strings.HasPrefix(v[1], strings.TrimSuffix(base.Packages["python"], path.Ext(base.Packages["python"]))+".") {
				c.apply("python", toolRelease{Version: v[1]})
			}
		}
	}
	return c
}
func releaseMetadata(ctx context.Context, address string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "commander")
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("release service returned %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return nil, errors.New("invalid release metadata")
	}
	return b, nil
}
func releaseJSON(ctx context.Context, address string, result any) error {
	b, e := releaseMetadata(ctx, address)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, result)
}
func latestTool(ctx context.Context, name string, c catalog) (toolRelease, error) {
	var r toolRelease
	var err error
	if pkg := npmPackages[name]; pkg != "" {
		err = releaseJSON(ctx, "https://registry.npmjs.org/"+url.PathEscape(pkg)+"/latest", &r)
		return r, err
	}
	if name == "azure" {
		var p struct{ Info toolRelease }
		err = releaseJSON(ctx, "https://pypi.org/pypi/azure-cli/json", &p)
		return p.Info, err
	}
	spec := c.Archives[name]
	if repo := map[string]string{"gh": "cli/cli", "compose": "docker/compose", "uv": "astral-sh/uv", "hcloud": "hetznercloud/cli"}[name]; repo != "" {
		var p struct {
			Tag    string `json:"tag_name"`
			Assets []struct{ Name, Digest string }
		}
		if err = releaseJSON(ctx, "https://api.github.com/repos/"+repo+"/releases/latest", &p); err != nil {
			return r, err
		}
		r.Version = strings.TrimPrefix(p.Tag, "v")
		want := strings.ReplaceAll(path.Base(spec.URL), spec.Version, r.Version)
		for _, asset := range p.Assets {
			if asset.Name == want {
				r.SHA256 = strings.TrimPrefix(asset.Digest, "sha256:")
				break
			}
		}
	} else {
		switch name {
		case "node":
			var rows []struct{ Version string }
			if err = releaseJSON(ctx, "https://nodejs.org/dist/index.json", &rows); err != nil {
				return r, err
			}
			major, _, _ := strings.Cut(spec.Version, ".")
			r.Version = spec.Version
			for _, row := range rows {
				v := strings.TrimPrefix(row.Version, "v")
				if strings.HasPrefix(v, major+".") && newerTool(v, r.Version) {
					r.Version = v
				}
			}
			if r.Version != spec.Version {
				asset := strings.ReplaceAll(path.Base(spec.URL), spec.Version, r.Version)
				b, e := releaseMetadata(ctx, "https://nodejs.org/dist/v"+r.Version+"/SHASUMS256.txt")
				if e != nil {
					return r, e
				}
				for _, line := range strings.Split(string(b), "\n") {
					f := strings.Fields(line)
					if len(f) == 2 && f[1] == asset {
						r.SHA256 = f[0]
						break
					}
				}
			}
		case "docker", "python":
			base := "https://www.python.org/ftp/python/"
			pattern := `href="([0-9.]+)/"`
			old := c.Packages["python"]
			if name == "docker" {
				base = "https://download.docker.com/linux/static/stable/" + machine + "/"
				pattern = `href="docker-([0-9.]+)\.tgz"`
				old = spec.Version
			}
			b, e := releaseMetadata(ctx, base)
			if e != nil {
				return r, e
			}
			r.Version = old
			series := strings.TrimSuffix(old, path.Ext(old)) + "."
			for _, m := range regexp.MustCompile(pattern).FindAllStringSubmatch(string(b), -1) {
				if (name == "docker" || strings.HasPrefix(m[1], series)) && newerTool(m[1], r.Version) {
					r.Version = m[1]
				}
			}
			if name == "docker" && newerTool(r.Version, old) {
				b, e = releaseMetadata(ctx, base+"docker-"+r.Version+".tgz.sha256")
				if e != nil {
					return r, e
				}
				if f := strings.Fields(string(b)); len(f) > 0 {
					r.SHA256 = f[0]
				}
			}
		case "gcloud":
			var meta toolRelease
			if err = releaseJSON(ctx, "https://dl.google.com/dl/cloudsdk/channels/rapid/components-2.json", &meta); err != nil {
				return r, err
			}
			r.Version = meta.Version
			if newerTool(r.Version, spec.Version) {
				b, e := releaseMetadata(ctx, "https://docs.cloud.google.com/sdk/docs/downloads-versioned-archives")
				if e != nil {
					return r, e
				}
				file := strings.Replace(path.Base(spec.URL), "-"+spec.Version, "", 1)
				for _, row := range strings.Split(string(b), "</tr>") {
					if strings.Contains(row, file) {
						r.SHA256 = regexp.MustCompile(`>\s*([0-9a-f]{64})\s*<`).FindString(row)
						r.SHA256 = strings.Trim(r.SHA256, "<> \t\n")
						if r.SHA256 != "" {
							break
						}
					}
				}
			}
		}
	}
	if !stableVersion.MatchString(r.Version) || newerTool(r.Version, spec.Version) && !checksum.MatchString(r.SHA256) {
		return r, errors.New("release metadata incomplete")
	}
	return r, nil
}

// Query independent upstreams together, then install in dependency order under
// the normal setup lock. One failed upstream never prevents the others updating.
func (a *app) refreshTools() error {
	unlock, err := a.lock("tool-refresh")
	if err != nil {
		return nil
	}
	defer unlock()
	current := a.installedCatalog(a.catalog)
	next := current.copy()
	used := []string{}
	names := map[string]bool{}
	for _, tool := range managedTools {
		if !a.toolUsed(tool) {
			continue
		}
		used = append(used, tool)
		for _, dep := range toolDependencies[tool] {
			if dep != "git" {
				names[dep] = true
			}
		}
	}
	type result struct {
		name string
		r    toolRelease
		err  error
	}
	ch := make(chan result, len(names))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for name := range names {
		go func() { r, e := a.latest(ctx, name, current); ch <- result{name, r, e} }()
	}
	var failures []error
	for range names {
		v := <-ch
		if v.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", v.name, v.err))
			continue
		}
		next.apply(v.name, v.r)
	}
	updater := *a
	updater.catalog = next
	updater.errOut = io.Discard
	for _, tool := range used {
		if err := updater.prepareTool(tool); err != nil {
			failures = append(failures, err)
		}
	}
	a.catalog = a.installedCatalog(current)
	return errors.Join(failures...)
}
