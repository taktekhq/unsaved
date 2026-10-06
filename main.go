// unsaved finds work that exists only on this machine: uncommitted files, unpushed commits,
// repositories with no remote, forgotten stashes, and directories of code that were never put in git.
// As a Claude Code Stop hook it keeps an agent from ending a turn with its own changes unsaved.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var version = "0.1.0"

const usage = `unsaved: find work that is not committed and pushed.

Usage:
  unsaved [flags] [ROOT...]   scan ROOTs (default: the roots file, else the current directory)
  unsaved hook                Claude Code Stop hook: reads the hook payload on stdin
  unsaved version

Flags:
  --json      print findings as JSON
  --push      push branches that are only ahead of their own upstream (fast-forward, never force)
  --depth N   how deep to look for repositories under each root (default 4)
  -q          print nothing when everything is saved

Config ($XDG_CONFIG_HOME/unsaved or ~/.config/unsaved):
  roots       default roots, one per line
  ignore      paths to skip, one per line (prefix match; ~ and globs allowed; # comments)
  hook-note   extra text appended to the hook's message, e.g. your team's rules

Exit status: 0 when everything is saved, 1 when something is not, 2 on error.
`

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "hook":
			if err := runHook(os.Stdin, os.Stdout, readList("ignore"), readFile("hook-note")); err != nil {
				fmt.Fprintln(os.Stderr, "unsaved:", err)
			}
			return
		case "version", "--version":
			fmt.Println("unsaved", version)
			return
		}
	}
	fl := flag.NewFlagSet("unsaved", flag.ContinueOnError)
	fl.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	asJSON := fl.Bool("json", false, "")
	push := fl.Bool("push", false, "")
	depth := fl.Int("depth", 4, "")
	quiet := fl.Bool("q", false, "")
	if err := fl.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return
		}
		os.Exit(2)
	}
	roots := fl.Args()
	if len(roots) == 0 {
		roots = readList("roots")
	}
	if len(roots) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "unsaved:", err)
			os.Exit(2)
		}
		roots = []string{wd}
	}
	fs := Scan(roots, *depth, readList("ignore"), *push)
	if *asJSON {
		if fs == nil {
			fs = []Finding{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(fs)
	} else if len(fs) > 0 {
		fmt.Print(render(fs))
	} else if !*quiet {
		fmt.Println("Everything is committed and pushed.")
	}
	// Pushed branches are saved now; anything else still needs a person.
	for _, f := range fs {
		if !f.Pushed {
			os.Exit(1)
		}
	}
}

func configDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "unsaved")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "unsaved")
}

func readFile(name string) string {
	b, _ := os.ReadFile(filepath.Join(configDir(), name))
	return string(b)
}

func readList(name string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(readFile(name)))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[1:])
	}
	return p
}

func tilde(p string) string {
	h, _ := os.UserHomeDir()
	if h != "" && (p == h || strings.HasPrefix(p, h+string(os.PathSeparator))) {
		return "~" + p[len(h):]
	}
	return p
}

// ignored reports whether p is, or is under, an ignore entry. Entries may be globs.
func ignored(p string, ignore []string) bool {
	for _, i := range ignore {
		i = filepath.Clean(expand(i))
		if p == i || strings.HasPrefix(p, i+string(os.PathSeparator)) {
			return true
		}
		if ok, _ := filepath.Match(i, p); ok {
			return true
		}
	}
	return false
}
