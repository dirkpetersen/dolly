package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dirkpetersen/dolly/internal/config"
	"github.com/dirkpetersen/dolly/internal/ldapconn"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// reloadConfig validates the config after fetch-cert wrote it; tests
// replace it.
var reloadConfig = config.Load

// pinEdit is one key fetch-cert sets in a config section.
type pinEdit struct {
	key  string // a key in the section, or a dotted path (users.base)
	node *yaml.Node
	// ifMissing: insert the key when the section lacks it (otherwise only
	// an existing key is changed).
	ifMissing bool
}

// pinSide is what fetch-cert (or dolly config) writes to one section of
// the config.
type pinSide struct {
	section string // "source", "target", ...; created if missing
	edits   []pinEdit
	remove  []string // keys removed first; new keys go where the first one was
	summary []string // what changes, for the confirmation
}

func flowList(vals []string, quoted bool) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range vals {
		s := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
		if quoted {
			s.Style = yaml.DoubleQuotedStyle
		}
		n.Content = append(n.Content, s)
	}
	return n
}

func scalar(tag, v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v} }

// writePins is dolly fetch-cert with --source/--target: fetch each chain
// (no credentials), then, after confirmation, write the given URLs and
// their leaf fingerprints to the config. --source replaces source.urls and
// source.tls_fingerprint, so the two lists always match; --target sets
// target.url, target.tls_fingerprint, and start_tls by the scheme. ca_file
// is removed on a side that gets pins (the two can't be combined).
func writePins(ctx context.Context, sources []string, targetURL, cfgFlag string, yes bool, stdout, stderr io.Writer) int {
	path, err := config.Find(cfgFlag)
	if err == nil {
		path, err = filepath.EvalSymlinks(path) // write the file itself, not replace a symlink
	}
	if err != nil {
		fmt.Fprintf(stderr, "dolly fetch-cert: %v\n", err)
		return exitError
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "dolly fetch-cert: %v\n", err)
		return exitError
	}
	pin := func(raw string) (string, bool) {
		chain, err := fetchAndPrint(ctx, raw, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "dolly fetch-cert: nothing was written\n")
			return "", false
		}
		fmt.Fprintln(stdout)
		return ldapconn.Fingerprint(chain[0]), true
	}
	list := func(v []string) string { return "[" + strings.Join(v, ", ") + "]" }

	var sides []pinSide
	if len(sources) > 0 {
		var urls, pins []string
		for _, u := range sources {
			fp, ok := pin(u)
			if !ok {
				return exitError
			}
			urls, pins = appendNew(urls, u), appendNew(pins, fp)
		}
		s := pinSide{section: "source", remove: []string{"ca_file"},
			edits: []pinEdit{{"urls", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: flowList(urls, false).Content}, true}, // block style, like the template
				{"tls_fingerprint", flowList(pins, true), true}},
			summary: []string{"source.urls: " + list(cfg.Source.URLs) + " -> " + list(urls), "source.tls_fingerprint: " + list(pins)}}
		if cfg.Source.CAFile != "" {
			s.summary = append(s.summary, "source.ca_file: removed (it can't be combined with pins)")
		}
		sides = append(sides, s)
	}
	if targetURL != "" {
		fp, ok := pin(targetURL)
		if !ok {
			return exitError
		}
		startTLS := ldapconn.Scheme(targetURL) == "ldap" // the fetch used StartTLS
		s := pinSide{section: "target", remove: []string{"ca_file"},
			edits: []pinEdit{{"url", scalar("!!str", targetURL), true}, {"tls_fingerprint", flowList([]string{fp}, true), true},
				{"start_tls", scalar("!!bool", fmt.Sprint(startTLS)), startTLS}},
			summary: []string{"target.url: " + cfg.Target.URL + " -> " + targetURL, "target.tls_fingerprint: " + list([]string{fp})}}
		if cfg.Target.StartTLS != startTLS {
			s.summary = append(s.summary, fmt.Sprintf("target.start_tls: %v -> %v", cfg.Target.StartTLS, startTLS))
		}
		if cfg.Target.CAFile != "" {
			s.summary = append(s.summary, "target.ca_file: removed (it can't be combined with pins)")
		}
		sides = append(sides, s)
	}

	fmt.Fprintf(stdout, "Changes to %s:\n", path)
	for _, s := range sides {
		for _, line := range s.summary {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
	}
	if !yes {
		if !stdinIsTTY() {
			fmt.Fprintf(stderr, "dolly fetch-cert: stdin is not a terminal; refusing to write the config without confirmation (use --yes)\n")
			return exitError
		}
		fmt.Fprintf(stdout, "Write these changes to %s? [y/N] ", path)
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Nothing written.")
			return exitOK
		}
	}

	orig, err := os.ReadFile(path)
	if err == nil {
		var edited []byte
		if edited, err = editConfig(orig, sides); err == nil {
			err = writeConfigFile(path, edited)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "dolly fetch-cert: writing %s: %v\n", path, err)
		return exitError
	}
	if _, err := reloadConfig(path); err != nil {
		if rerr := writeConfigFile(path, orig); rerr != nil {
			fmt.Fprintf(stderr, "dolly fetch-cert: the new config is invalid (%v), and restoring the original failed: %v\n", err, rerr)
			return exitError
		}
		fmt.Fprintf(stderr, "dolly fetch-cert: the new config is invalid, so the original was restored: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "Wrote %s.\n", path)
	return exitOK
}

func appendNew(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// editConfig edits the YAML node tree, so comments, key order, and every
// other value stay as they are: per section, the remove keys go, an
// existing key is changed in place (keeping its comments), and a missing
// one is inserted where the first removed key was, or at the end.
func editConfig(data []byte, sides []pinSide) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the config is not a YAML mapping")
	}
	for _, s := range sides {
		sec := mapValue(doc.Content[0], s.section)
		if sec == nil {
			sec = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			doc.Content[0].Content = append(doc.Content[0].Content, scalar("!!str", s.section), sec)
		}
		if sec.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s is not a section", s.section)
		}
		at := -1 // where the first removed key was
		var kept []*yaml.Node
		for i := 0; i+1 < len(sec.Content); i += 2 {
			if slices.Contains(s.remove, sec.Content[i].Value) {
				if at < 0 {
					at = len(kept)
				}
				continue
			}
			kept = append(kept, sec.Content[i], sec.Content[i+1])
		}
		sec.Content = kept
		for _, e := range s.edits {
			m, key := sec, e.key
			for strings.Contains(key, ".") {
				var parent string
				parent, key, _ = strings.Cut(key, ".")
				next := mapValue(m, parent)
				if next == nil {
					next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
					m.Content = append(m.Content, scalar("!!str", parent), next)
				}
				m = next
			}
			if v := mapValue(m, key); v != nil {
				head, line, foot := v.HeadComment, v.LineComment, v.FootComment
				*v = *e.node
				v.HeadComment, v.LineComment, v.FootComment = head, line, foot
				continue
			}
			if !e.ifMissing {
				continue
			}
			pos := len(m.Content)
			if m == sec && at >= 0 {
				pos, at = at, at+2
			}
			kv := []*yaml.Node{scalar("!!str", key), e.node}
			m.Content = append(m.Content[:pos:pos], append(kv, m.Content[pos:]...)...)
		}
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return []byte(b.String()), enc.Close()
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// writeConfigFile replaces (or creates) path atomically: a temp file in
// the same directory, mode 0600 (or the original mode if stricter), fsync,
// rename. It also writes the password files of dolly config.
func writeConfigFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode &= st.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	err = errors.Join(err, f.Chmod(mode), f.Sync(), f.Close())
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
