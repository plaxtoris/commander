package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const memoryLimit = 4000

// Memory is local user data. Only the active agent decides what to remember.
// The revision check prevents one session from replacing another's changes.
func (a *app) memory(args []string) error {
	if len(args) != 0 && (len(args) != 2 || args[0] != "write") {
		return errors.New("usage: commander memory [write REVISION]; write reads the complete UTF-8 bullet list from stdin")
	}
	var next []byte
	if len(args) != 0 {
		var err error
		next, err = io.ReadAll(io.LimitReader(a.in, memoryLimit*utf8.UTFMax+1))
		if err != nil {
			return err
		}
		if err = validateMemory(next); err != nil {
			return err
		}
	}
	if err := a.ensure(); err != nil {
		return err
	}
	unlock, err := a.lock("memory")
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(a.root, "MEMORY.md")
	if err = noSymlinks(a.root, path); err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	revision := fmt.Sprintf("%x", sha256.Sum256(current))
	if len(args) == 0 {
		_, err = fmt.Fprintf(a.out, "revision: %s\ncharacters: %d/%d\n\n%s", revision, utf8.RuneCount(current), memoryLimit, current)
		return err
	}
	if args[1] != revision {
		return errors.New("memory changed; run commander memory again, merge your changes and retry with its revision")
	}
	// Existing installations keep their own .gitignore. Add only this exclusion.
	ignore := filepath.Join(a.root, ".gitignore")
	if err = noSymlinks(a.root, ignore); err != nil {
		return err
	}
	data, err := os.ReadFile(ignore)
	if err != nil {
		return err
	}
	if !strings.Contains("\n"+strings.ReplaceAll(string(data), "\r\n", "\n")+"\n", "\n/MEMORY.md\n") {
		data = append(data, []byte("\n/MEMORY.md\n")...)
		if err = atomicWrite(ignore, data, 0644); err != nil {
			return err
		}
	}
	if string(current) == string(next) {
		fmt.Fprintln(a.out, "memory unchanged")
		return nil
	}
	if err = atomicWrite(path, next, 0600); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "memory saved: %d/%d characters\n", utf8.RuneCount(next), memoryLimit)
	return nil
}

func validateMemory(data []byte) error {
	if len(data) > memoryLimit*utf8.UTFMax {
		return errors.New("memory exceeds 4000 characters; consolidate to about 3500 and retry")
	}
	if !utf8.Valid(data) {
		return errors.New("memory must be valid UTF-8")
	}
	if count := utf8.RuneCount(data); count > memoryLimit {
		return fmt.Errorf("memory has %d characters; maximum is %d; consolidate to about 3500 and retry", count, memoryLimit)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' {
			return errors.New("memory must contain plain text without control characters")
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !(strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "- ")) || strings.TrimSpace(line[2:]) == "" {
			return errors.New("memory must contain one short statement per bullet, starting with '* ' or '- '; no headings or nested lists")
		}
	}
	return nil
}
