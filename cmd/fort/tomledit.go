package main

import (
	"fmt"
	"strings"
)

// Text-level edits of a manifest (forts/<host>.toml): the file is the
// fort's hand-written declaration, with the fort's history in its
// comments, so a verb that adds a table appends lines and a verb that
// changes one value rewrites that line — never the file through an
// encoder, which would drop every comment.

// tomlHeader is the table a header line names — `[links.x]`,
// `[ links.x ]` and `[links.x]   # a comment` all name links.x — or ""
// for any other line, an array-of-tables header `[[x]]` included (no
// verb here edits one).
func tomlHeader(line string) string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return ""
	}
	end := strings.Index(t, "]")
	if end < 0 {
		return ""
	}
	if rest := strings.TrimSpace(t[end+1:]); rest != "" && !strings.HasPrefix(rest, "#") {
		return ""
	}
	return strings.TrimSpace(t[1:end])
}

// tomlTableRange is the line range [start, end) of the table whose
// header names <table> (tomlHeader): the header line through the last
// line before the next table header (or the end). start is -1 when
// absent.
func tomlTableRange(lines []string, table string) (start, end int) {
	start = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if tomlHeader(l) == table {
			start = i
			continue
		}
		if start >= 0 && strings.HasPrefix(t, "[") {
			return start, i
		}
	}
	if start < 0 {
		return -1, -1
	}
	return start, len(lines)
}

// tomlSetInTable sets `key = value` in the table (value already
// TOML-quoted): the first line of the table that sets the key is
// rewritten, its trailing comment kept; a key the table lacks lands
// after the table's last key line. A table that does not exist is
// appended at the end of the file with the key as its first line.
func tomlSetInTable(text, table, key, value string) string {
	lines := strings.Split(text, "\n")
	start, end := tomlTableRange(lines, table)
	if start < 0 {
		return appendBlock(text, "\n["+table+"]\n"+key+" = "+value)
	}
	last := start
	for i := start + 1; i < end; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		last = i
		k, _, ok := strings.Cut(t, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		comment := ""
		if j := strings.Index(lines[i], "#"); j >= 0 {
			comment = "   " + lines[i][j:]
		}
		lines[i] = key + " = " + value + comment
		return strings.Join(lines, "\n")
	}
	out := append([]string{}, lines[:last+1]...)
	out = append(out, key+" = "+value)
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n")
}

// tomlHasTable says whether the file has the table.
func tomlHasTable(text, table string) bool {
	s, _ := tomlTableRange(strings.Split(text, "\n"), table)
	return s >= 0
}

// tomlString quotes a TOML basic string.
func tomlString(s string) string {
	return fmt.Sprintf("%q", s)
}
