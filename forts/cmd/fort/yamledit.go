package main

import (
	"fmt"
	"strings"
)

// Text-level edits for the two "append a line to an existing file" cases
// fort add is allowed (docs/forts/fort-configuration-contracts.md: new files + appended list
// entries, never a rewrite). Working on text rather than a YAML tree
// keeps every comment and the author's formatting byte-for-byte; the
// files are ours, so the shapes assumed here are the shapes we render.

// appendListItem inserts `  - <item>` as the last entry of the top-level
// block sequence under <key> (e.g. resources:). The block runs from the
// key line through every following indented or comment line.
func appendListItem(text, key, item string) (string, error) {
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
	entry := "  - " + item
	out := append([]string{}, lines[:end+1]...)
	out = append(out, entry)
	out = append(out, lines[end+1:]...)
	return strings.Join(out, "\n"), nil
}

// appendBlock adds a top-level block at the end of the document (a new
// `patches:` list, a new entry under a trailing `secrets:` map). The
// block text is inserted verbatim before the final newline.
func appendBlock(text, block string) string {
	text = strings.TrimRight(text, "\n") + "\n"
	return text + strings.TrimRight(block, "\n") + "\n"
}

// appendPatch registers a kustomize JSON6902 patch file against a target,
// creating the `patches:` block (with the given comment) on first use.
func appendPatch(text, comment, path, group, version, kind, name, namespace string) string {
	entry := fmt.Sprintf(`  - path: %s
    target:
      group: %s
      version: %s
      kind: %s
      name: %s
      namespace: %s
`, path, group, version, kind, name, namespace)
	if strings.Contains("\n"+text, "\npatches:\n") {
		return appendBlock(text, entry)
	}
	return appendBlock(text, comment+"patches:\n"+entry)
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

// appendMapEntryUnder is appendMapEntry scoped to one section: the anchor
// is searched only after the line that is exactly <section>, for files
// that carry the same anchor twice (the rustfs contract's two `keys:`).
func appendMapEntryUnder(text, section, anchor, entry string) (string, error) {
	lines := strings.Split(text, "\n")
	at := -1
	for i, l := range lines {
		if l == section {
			at = i
			break
		}
	}
	if at < 0 {
		return "", fmt.Errorf("no %q line", section)
	}
	head := strings.Join(lines[:at+1], "\n")
	tail, err := appendMapEntry(strings.Join(lines[at+1:], "\n"), anchor, entry)
	if err != nil {
		return "", err
	}
	return head + "\n" + tail, nil
}

// appendAfterLast inserts <line> after the last line that starts with
// <prefix> (whitespace included) — the bucket Job's `rc bucket create`
// run, where order is meaningless and the shape is one line per app.
func appendAfterLast(text, prefix, line string) (string, error) {
	lines := strings.Split(text, "\n")
	at := -1
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			at = i
		}
	}
	if at < 0 {
		return "", fmt.Errorf("no line starting with %q", prefix)
	}
	out := append([]string{}, lines[:at+1]...)
	out = append(out, line)
	out = append(out, lines[at+1:]...)
	return strings.Join(out, "\n"), nil
}
