package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// A Finding is one piece of work that only exists on this machine.
type Finding struct {
	Path   string   `json:"path"`             // repo or directory
	Kind   string   `json:"kind"`             // dirty, unpushed, no-remote, stash, not-in-git
	Count  int      `json:"count"`            // files, commits or stashes
	Branch string   `json:"branch,omitempty"` // for unpushed
	Files  []string `json:"files,omitempty"`  // a sample, relative to Path
	Pushed bool     `json:"pushed,omitempty"` // --push sent it
}

// Directories never worth descending into.
var skipNames = map[string]bool{
	"node_modules": true, "vendor": true, "venv": true, ".venv": true, "__pycache__": true,
	"Library": true, "Applications": true, "Pictures": true, "Music": true, "Movies": true,
	"target": true, "dist": true, "build": true,
}

var sourceExt = map[string]bool{
	".go": true, ".py": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true,
	".jsx": true, ".sh": true, ".bash": true, ".zsh": true, ".rb": true, ".rs": true, ".swift": true,
	".kt": true, ".java": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".php": true,
	".lua": true, ".pl": true, ".ex": true, ".exs": true, ".el": true, ".fish": true,
}

// isSource reports whether a file looks like code someone wrote: a known extension,
// or an executable with no extension that starts with a shebang.
func isSource(path string, d fs.DirEntry) bool {
	ext := filepath.Ext(d.Name())
	if sourceExt[ext] {
		return true
	}
	if ext != "" {
		return false
	}
	info, err := d.Info()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Size() > 1<<20 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 2)
	n, _ := f.Read(b)
	return n == 2 && string(b) == "#!"
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return strings.TrimRight(out.String(), "\n"), err
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// dirtyFiles lists uncommitted paths (relative to the worktree top), untracked files included.
func dirtyFiles(top string) []string {
	out, err := git(top, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || out == "" {
		return nil
	}
	var files []string
	parts := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if len(p) < 4 {
			continue
		}
		files = append(files, p[3:])
		if p[0] == 'R' || p[0] == 'C' {
			i++ // the next entry is the rename's old path
		}
	}
	return files
}

type branch struct {
	name, remote, remoteRef, track string
}

func branches(top string) []branch {
	out, _ := git(top, "for-each-ref", "--format=%(refname:short)\t%(upstream:remotename)\t%(upstream:remoteref)\t%(upstream:track)", "refs/heads")
	var bs []branch
	for _, l := range lines(out) {
		f := strings.Split(l, "\t")
		if len(f) == 4 {
			bs = append(bs, branch{f[0], f[1], f[2], f[3]})
		}
	}
	return bs
}

// unpushed counts commits on a branch that no remote has.
func unpushed(top, b string) int {
	out, err := git(top, "rev-list", "--count", "refs/heads/"+b, "--not", "--remotes")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(out)
	return n
}

func sample(files []string, n int) []string {
	if len(files) > n {
		return append(append([]string{}, files[:n]...), "…")
	}
	return files
}

// checkRepo reports a worktree's uncommitted files. Branches, remotes and stashes are shared by
// every worktree of a repository, so they are checked once per common git dir (seen).
func checkRepo(top string, seen map[string]bool, push bool) []Finding {
	var fs []Finding
	if files := dirtyFiles(top); len(files) > 0 {
		fs = append(fs, Finding{Path: top, Kind: "dirty", Count: len(files), Files: sample(files, 8)})
	}
	common, err := git(top, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || seen[common] {
		return fs
	}
	seen[common] = true
	if remotes, _ := git(top, "remote"); remotes == "" {
		n, _ := git(top, "rev-list", "--count", "--all")
		c, _ := strconv.Atoi(n)
		return append(fs, Finding{Path: top, Kind: "no-remote", Count: c})
	}
	for _, b := range branches(top) {
		n := unpushed(top, b.name)
		if n == 0 {
			continue
		}
		f := Finding{Path: top, Kind: "unpushed", Count: n, Branch: b.name}
		// Only a plain fast-forward to the branch's own upstream is pushed: never a new
		// remote branch, never a force.
		if push && b.remote != "" && strings.HasPrefix(b.track, "[ahead") && !strings.Contains(b.track, "behind") {
			_, err := git(top, "push", "--quiet", b.remote, "refs/heads/"+b.name+":"+b.remoteRef)
			f.Pushed = err == nil
		}
		fs = append(fs, f)
	}
	if out, _ := git(top, "stash", "list"); out != "" {
		fs = append(fs, Finding{Path: top, Kind: "stash", Count: len(lines(out))})
	}
	return fs
}

// Scan walks roots for git repositories and for directories of code that are in none.
func Scan(roots []string, depth int, ignore []string, push bool) []Finding {
	var out []Finding
	seen := map[string]bool{}
	for _, root := range roots {
		root = filepath.Clean(expand(root))
		base := strings.Count(root, string(os.PathSeparator))
		var loose []string // code directories outside git; their subdirectories aren't reported again
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || skipNames[name] || ignored(p, ignore)) {
				return fs.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				out = append(out, checkRepo(p, seen, push)...)
				return fs.SkipDir
			}
			if strings.Count(p, string(os.PathSeparator))-base >= depth {
				return fs.SkipDir
			}
			for _, l := range loose {
				if strings.HasPrefix(p, l+string(os.PathSeparator)) {
					return nil
				}
			}
			entries, _ := os.ReadDir(p)
			var src []string
			for _, e := range entries {
				if !e.IsDir() && isSource(filepath.Join(p, e.Name()), e) {
					src = append(src, e.Name())
				}
			}
			if len(src) > 0 && !insideRepo(p) {
				loose = append(loose, p)
				out = append(out, Finding{Path: p, Kind: "not-in-git", Count: len(src), Files: sample(src, 8)})
			}
			return nil
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// repoTop returns the worktree that contains path, or "" when it is in none.
func repoTop(path string) string {
	for d := path; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

func insideRepo(dir string) bool { return repoTop(dir) != "" }
