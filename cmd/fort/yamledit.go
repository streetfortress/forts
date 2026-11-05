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

// appendPatch adds a JSON6902 patch entry to a kustomization: inside
// the existing patches: list, or as a new block (with its comment) at
// the end. The entry goes inside the list, not at the end of the file:
// appendBlock would put it under whatever block follows — `kustomize
// build` then reads `path` as a field of a trailing configMapGenerator
// and fails the whole apps layer with `unknown field "path"`.
func appendPatch(text, comment, path, group, version, kind, name, namespace string) string {
	entry := fmt.Sprintf(`  - path: %s
    target:
      group: %s
      version: %s
      kind: %s
      name: %s
      namespace: %s
`, path, group, version, kind, name, namespace)
	if out, err := insertIntoBlock(text, "patches", entry); err == nil {
		return out
	}
	return appendBlock(text, comment+"patches:\n"+entry)
}

// removeListItem deletes the `  - <item>` line of the top-level block
// under <key> — the inverse of appendListItem, for fort remove. An
// entry that is not there is an error: the tree is not the shape the
// verb wrote, and the operator must look.
func removeListItem(text, key, item string) (string, error) {
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
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") {
			break
		}
		if l == "  - "+item {
			return strings.Join(append(lines[:i:i], lines[i+1:]...), "\n"), nil
		}
	}
	return "", fmt.Errorf("no `- %s` entry under %s:", item, key)
}

// removePatchEntry deletes the JSON6902 patch entry whose path is
// <path> from the top-level patches: list — its `  - path:` line and
// the deeper-indented lines beneath it — the inverse of appendPatch.
// When the list is then empty, the patches: key goes too, with the
// comment lines directly above it, so the file is the shape appendPatch
// starts from (appendBlock re-adds both on the next listener).
func removePatchEntry(text, path string) (string, error) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if l == "patches:" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no top-level patches: block")
	}
	end := start // the last line the block owns
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") {
			break
		}
		if strings.HasPrefix(l, " ") && strings.TrimSpace(l) != "" {
			end = i
		}
	}
	entry := -1
	for i := start + 1; i <= end; i++ {
		if lines[i] == "  - path: "+path {
			entry = i
			break
		}
	}
	if entry < 0 {
		return "", fmt.Errorf("no patch entry for %s under patches:", path)
	}
	next := entry + 1
	for next <= end && strings.HasPrefix(lines[next], "    ") {
		next++
	}
	out := append(lines[:entry:entry], lines[next:]...)
	// The list is empty when no `  - ` item remains in the block.
	empty := true
	for i := start + 1; i < len(out); i++ {
		l := out[i]
		if l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") {
			break
		}
		if strings.HasPrefix(l, "  - ") {
			empty = false
			break
		}
	}
	if !empty {
		return strings.Join(out, "\n"), nil
	}
	first := start
	for first > 0 && strings.HasPrefix(out[first-1], "#") {
		first--
	}
	return strings.Join(append(out[:first:first], out[start+1:]...), "\n"), nil
}
