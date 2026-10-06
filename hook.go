package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// hookInput is what Claude Code sends a Stop hook on stdin.
type hookInput struct {
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// Tools whose input names a file the agent wrote.
var writeTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// touchedFiles returns every file the session wrote with an editing tool, as absolute paths.
func touchedFiles(transcript, cwd string) []string {
	f, err := os.Open(transcript)
	if err != nil {
		return nil
	}
	defer f.Close()
	set := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"tool_use"`) {
			continue
		}
		var entry struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}
		var blocks []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				FilePath     string `json:"file_path"`
				NotebookPath string `json:"notebook_path"`
			} `json:"input"`
		}
		if json.Unmarshal(entry.Message.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type != "tool_use" || !writeTools[b.Name] {
				continue
			}
			p := b.Input.FilePath
			if p == "" {
				p = b.Input.NotebookPath
			}
			if p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(cwd, p)
			}
			set[filepath.Clean(p)] = true
		}
	}
	var out []string
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Places an agent writes on purpose without meaning to keep anything.
var scratchPrefixes = func() []string {
	ps := []string{"/tmp", "/private/tmp", "/private/var/folders", "/var/folders", "/dev"}
	if t := os.TempDir(); t != "" {
		ps = append(ps, filepath.Clean(t))
	}
	if h, err := os.UserHomeDir(); err == nil {
		ps = append(ps, filepath.Join(h, ".claude"))
	}
	return ps
}

// hookReport checks the files a session touched and says what is still unsaved.
func hookReport(touched []string, ignore []string) []Finding {
	ignore = append(append([]string{}, ignore...), scratchPrefixes()...)
	byRepo := map[string][]string{}
	var loose []string
	for _, p := range touched {
		if ignored(p, ignore) {
			continue
		}
		top := repoTop(filepath.Dir(p))
		if top == "" {
			if _, err := os.Stat(p); err == nil {
				loose = append(loose, p)
			}
			continue
		}
		byRepo[top] = append(byRepo[top], p)
	}
	var out []Finding
	tops := make([]string, 0, len(byRepo))
	for t := range byRepo {
		tops = append(tops, t)
	}
	sort.Strings(tops)
	for _, top := range tops {
		dirty := map[string]bool{}
		for _, f := range dirtyFiles(top) {
			dirty[filepath.Join(top, f)] = true
		}
		// Only this session's files: another session may be mid-change in the same repo.
		var mine []string
		for _, p := range byRepo[top] {
			if dirty[p] {
				rel, _ := filepath.Rel(top, p)
				mine = append(mine, rel)
			}
		}
		if len(mine) > 0 {
			out = append(out, Finding{Path: top, Kind: "dirty", Count: len(mine), Files: sample(mine, 8)})
		}
		if remotes, _ := git(top, "remote"); remotes == "" {
			out = append(out, Finding{Path: top, Kind: "no-remote"})
			continue
		}
		if b, err := git(top, "branch", "--show-current"); err == nil && b != "" {
			if n := unpushed(top, b); n > 0 {
				out = append(out, Finding{Path: top, Kind: "unpushed", Count: n, Branch: b})
			}
		}
	}
	for _, p := range loose {
		out = append(out, Finding{Path: filepath.Dir(p), Kind: "not-in-git", Count: 1, Files: []string{filepath.Base(p)}})
	}
	return out
}

const hookAdvice = `Before you finish: review the diff, commit with a message that says why, and push.
- No remote yet: create the repository (e.g. gh repo create) and push to it.
- A file outside git that is worth keeping belongs in a repository. If it is a tool, give it its own
  repository with a README, so it can be used outside this project.
- Never commit secrets or private data. What must stay local goes in .gitignore.
- If something is deliberately left uncommitted, say so in your reply and stop.`

func runHook(stdin io.Reader, stdout io.Writer, ignore []string, note string) error {
	if v := strings.ToLower(os.Getenv("UNSAVED_HOOK")); v == "off" || v == "0" || v == "false" {
		return nil
	}
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil // never block a session on a malformed payload
	}
	if in.StopHookActive {
		return nil // already asked once this stop; don't loop
	}
	fs := hookReport(touchedFiles(in.TranscriptPath, in.Cwd), ignore)
	if len(fs) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("This session left work that is not committed and pushed:\n\n")
	b.WriteString(render(fs))
	b.WriteString("\n" + hookAdvice)
	if note != "" {
		b.WriteString("\n\n" + strings.TrimSpace(note))
	}
	return json.NewEncoder(stdout).Encode(map[string]string{"decision": "block", "reason": b.String()})
}

// render prints findings grouped by path, the way a person reads them.
func render(fs []Finding) string {
	var b strings.Builder
	last := ""
	for _, f := range fs {
		if f.Path != last {
			if last != "" {
				b.WriteString("\n")
			}
			b.WriteString(tilde(f.Path) + "\n")
			last = f.Path
		}
		b.WriteString("  - " + describe(f) + "\n")
	}
	return b.String()
}

func describe(f Finding) string {
	files := ""
	if len(f.Files) > 0 {
		files = ": " + strings.Join(f.Files, ", ")
	}
	switch f.Kind {
	case "dirty":
		return plural(f.Count, "uncommitted file") + files
	case "unpushed":
		s := plural(f.Count, "unpushed commit") + " on " + f.Branch
		if f.Pushed {
			s += " (pushed now)"
		}
		return s
	case "no-remote":
		if f.Count > 0 {
			return "no remote: " + plural(f.Count, "commit") + " exist only here"
		}
		return "no remote: nothing here is pushed anywhere"
	case "stash":
		return plural(f.Count, "stash", "stashes")
	case "not-in-git":
		return "not in git, " + plural(f.Count, "source file") + files
	}
	return fmt.Sprintf("%s (%d)", f.Kind, f.Count)
}

func plural(n int, one string, many ...string) string {
	if n == 1 {
		return "1 " + one
	}
	if len(many) > 0 {
		return strconv.Itoa(n) + " " + many[0]
	}
	return strconv.Itoa(n) + " " + one + "s"
}
