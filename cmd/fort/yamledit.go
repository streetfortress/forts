package main

import (
	"fmt"
	"strings"
)

// Text-level edits for the "append a line to an existing file" cases an
// author verb is allowed (new files + appended list entries, never a
// rewrite). Working on text rather than a YAML tree
// keeps every comment and the author's formatting byte-for-byte; the
// files are ours, so the shapes assumed here are the shapes we render.

// insertIntoBlock inserts <entry> — one line, or several already
// indented — as the last item of the top-level block under <key> (e.g.
// resources:, patches:). The block runs from the key line through every
// following indented or comment line, so the entry lands inside the
// block even when another top-level block follows it.
func insertIntoBlock(text, key, entry string) (string, error) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if l == key+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no top-level %s: block", key)
	}
	// end: the last line the block owns. A blank line or a top-level
	// comment between two blocks belongs to neither, so the scan passes
	// over it and the entry lands against the block's own last item.
	end := start
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if l == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "#") {
			if strings.TrimSpace(l) != "" && strings.HasPrefix(l, " ") {
				end = i
			}
			continue
		}
		break
	}
	out := append([]string{}, lines[:end+1]...)
	out = append(out, strings.Split(strings.TrimRight(entry, "\n"), "\n")...)
	out = append(out, lines[end+1:]...)
	return strings.Join(out, "\n"), nil
}

// appendListItem inserts `  - <item>` as the last entry of the top-level
// block sequence under <key> (e.g. resources:).
func appendListItem(text, key, item string) (string, error) {
	return insertIntoBlock(text, key, "  - "+item)
}

// appendBlock adds a top-level block at the end of the document (a new
// `patches:` list, a new entry under a trailing `secrets:` map). The
// block text is inserted verbatim before the final newline.
func appendBlock(text, block string) string {
	text = strings.TrimRight(text, "\n") + "\n"
	return text + strings.TrimRight(block, "\n") + "\n"
}

// appendMapEntry inserts `<indent><key>: <value>` as the last entry of the
// mapping whose key line is exactly <anchor> (e.g. "    keys:"), i.e.
// after the last line indented deeper than the anchor.
func appendMapEntry(text, anchor, entry string) (string, error) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if l == anchor {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no %q line", anchor)
	}
	depth := len(anchor) - len(strings.TrimLeft(anchor, " "))
	end := start
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" {
			continue
		}
		if len(l)-len(strings.TrimLeft(l, " ")) <= depth {
			break
		}
		end = i
	}
	out := append([]string{}, lines[:end+1]...)
	out = append(out, entry)
	out = append(out, lines[end+1:]...)
	return strings.Join(out, "\n"), nil
}

// ensureClusterVar adds `  <key>: <value>` to a cluster-vars ConfigMap's
// data: map when no line sets <key>, and returns the text unchanged when
// one does — whatever its value: the file is the estate's, and `fort
// check` is what judges the value (fleet.go). The one case scaffold
// edits an existing cluster-vars.yaml (forts#50).
func ensureClusterVar(text, key, value string) (string, error) {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), key+":") && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			return text, nil
		}
	}
	return appendMapEntry(text, "data:", "  "+key+": "+value)
}
