package main

import (
	"fmt"
	"strings"
)

// Text-level edits of a host's default.nix — the one generated file
// scaffold never overwrites (the manifest seeds it, the estate refines
// it), and so the one a later declaration in the manifest has to be
// appended to: a link ([links.<name>], forts#55) on a hub that exists.
// Working on text keeps every comment and the estate's hand edits
// byte-for-byte; the shapes assumed are the ones our templates render,
// and a file that lost them says so instead of being guessed at.

// nixEnsureImport inserts `    <line>` — a module, with or without a
// trailing comment — as the last entry of the top-level `imports = [`
// list, unless a line of the list already names the module (whatever
// its comment). The list runs from the `imports = [` line to the first
// `];` after it.
func nixEnsureImport(text, line string) (string, error) {
	module := strings.Fields(line)[0]
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "imports = [" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no `imports = [` list")
	}
	indent := strings.TrimSuffix(lines[start], strings.TrimLeft(lines[start], " "))
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "];" {
			out := append([]string{}, lines[:i]...)
			out = append(out, indent+"  "+line)
			out = append(out, lines[i:]...)
			return strings.Join(out, "\n"), nil
		}
		if fields := strings.Fields(lines[i]); len(fields) > 0 && fields[0] == module {
			return text, nil
		}
	}
	return "", fmt.Errorf("`imports = [` list is not closed by `];`")
}

// nixSetsAttr says whether a line of code in text sets the attribute
// path — `<path> = …`, `<path>.<key> = …`, or the last label quoted
// (`sfi.links."staging" = …`). A comment line that mentions the path is
// not it, and neither is a longer path this one is a prefix of
// (sfi.links.prod beside sfi.links.production).
func nixSetsAttr(text, path string) bool {
	forms := []string{path}
	if i := strings.LastIndex(path, "."); i >= 0 {
		forms = append(forms, path[:i+1]+`"`+path[i+1:]+`"`)
	}
	for _, l := range strings.Split(text, "\n") {
		code := strings.TrimSpace(l)
		if strings.HasPrefix(code, "#") {
			continue
		}
		for _, f := range forms {
			rest, ok := strings.CutPrefix(code, f)
			if ok && rest != "" && strings.ContainsRune(" \t=.", rune(rest[0])) {
				return true
			}
		}
	}
	return false
}

// nixEnsureBlock appends block (an attribute set entry, already
// indented, with its comment) before the file's closing `}`, unless a
// line of the file already sets path (nixSetsAttr) — so a block that is
// there under a hand edit is kept, never doubled.
func nixEnsureBlock(text, path, block string) (string, error) {
	if nixSetsAttr(text, path) {
		return text, nil
	}
	lines := strings.Split(text, "\n")
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if lines[i] == "}" {
			end = i
		}
		break
	}
	if end < 0 {
		return "", fmt.Errorf("the file does not end in a closing `}`")
	}
	block = strings.TrimRight(block, "\n")
	out := append([]string{}, lines[:end]...)
	// One blank line between the last entry and the block, never two.
	if end > 0 && strings.TrimSpace(lines[end-1]) != "" {
		out = append(out, "")
	}
	out = append(out, strings.Split(block, "\n")...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}
