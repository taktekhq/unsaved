package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture builds: clean (pushed), dirty (uncommitted file), ahead (unpushed commit),
// lonely (no remote) and loose (code outside git), all under one root.
func fixture(t *testing.T) string {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	bare := filepath.Join(root, "remote.git")
	run(t, root, "git", "init", "-q", "--bare", "-b", "main", bare)
	for _, name := range []string{"clean", "dirty", "ahead"} {
		dir := filepath.Join(root, "work", name)
		os.MkdirAll(dir, 0o755)
		run(t, dir, "git", "init", "-q", "-b", name)
		write(t, filepath.Join(dir, "a.go"), "package a\n")
		run(t, dir, "git", "add", ".")
		run(t, dir, "git", "commit", "-q", "-m", "a")
		run(t, dir, "git", "remote", "add", "origin", bare)
		run(t, dir, "git", "push", "-q", "-u", "origin", name)
	}
	write(t, filepath.Join(root, "work/dirty/b.go"), "package a\n")
	write(t, filepath.Join(root, "work/ahead/a.go"), "package a // changed\n")
	run(t, filepath.Join(root, "work/ahead"), "git", "commit", "-q", "-am", "b")

	lonely := filepath.Join(root, "work", "lonely")
	os.MkdirAll(lonely, 0o755)
	run(t, lonely, "git", "init", "-q", "-b", "main")
	write(t, filepath.Join(lonely, "x.py"), "print(1)\n")
	run(t, lonely, "git", "add", ".")
	run(t, lonely, "git", "commit", "-q", "-m", "x")

	write(t, filepath.Join(root, "work/loose/tool.mjs"), "console.log(1)\n")
	write(t, filepath.Join(root, "work/loose/deeper/more.py"), "print(2)\n")
	write(t, filepath.Join(root, "work/notes/readme.md"), "just notes\n")
	return filepath.Join(root, "work")
}

func kinds(fs []Finding) map[string]string {
	m := map[string]string{}
	for _, f := range fs {
		m[filepath.Base(f.Path)] += f.Kind + " "
	}
	return m
}

func TestScan(t *testing.T) {
	work := fixture(t)
	got := kinds(Scan([]string{work}, 4, nil, false))
	want := map[string]string{
		"dirty":  "dirty ",
		"ahead":  "unpushed ",
		"lonely": "no-remote ",
		"loose":  "not-in-git ",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"clean", "notes", "deeper"} {
		if got[k] != "" {
			t.Errorf("%s should be clean, got %q", k, got[k])
		}
	}
}

func TestScanIgnore(t *testing.T) {
	work := fixture(t)
	got := kinds(Scan([]string{work}, 4, []string{filepath.Join(work, "loose"), filepath.Join(work, "lone*")}, false))
	if got["loose"] != "" || got["lonely"] != "" {
		t.Errorf("ignored paths reported: %v", got)
	}
}

func TestPush(t *testing.T) {
	work := fixture(t)
	fs := Scan([]string{work}, 4, nil, true)
	for _, f := range fs {
		if f.Kind == "unpushed" && !f.Pushed {
			t.Errorf("%s not pushed", f.Path)
		}
	}
	if got := kinds(Scan([]string{work}, 4, nil, false)); got["ahead"] != "" {
		t.Errorf("still unpushed after --push: %q", got["ahead"])
	}
}

func transcript(t *testing.T, files ...string) string {
	var b strings.Builder
	b.WriteString(`{"type":"user","message":{"content":"hi"}}` + "\n")
	for _, f := range files {
		line, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Write", "input": map[string]any{"file_path": f}},
		}}})
		b.Write(append(line, '\n'))
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, b.String())
	return p
}

func hook(t *testing.T, payload map[string]any) map[string]string {
	t.Helper()
	old := scratchPrefixes
	scratchPrefixes = func() []string { return nil } // fixtures live in the temp dir
	defer func() { scratchPrefixes = old }()
	in, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := runHook(bytes.NewReader(in), &out, nil, "Team rule: open-source the tool."); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return nil
	}
	var res map[string]string
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHookBlocksOnOwnFiles(t *testing.T) {
	work := fixture(t)
	res := hook(t, map[string]any{
		"transcript_path": transcript(t, filepath.Join(work, "dirty/b.go"), filepath.Join(work, "loose/tool.mjs")),
		"cwd":             work,
	})
	if res["decision"] != "block" {
		t.Fatalf("want block, got %v", res)
	}
	for _, s := range []string{"b.go", "not in git", "tool.mjs", "Team rule"} {
		if !strings.Contains(res["reason"], s) {
			t.Errorf("reason lacks %q:\n%s", s, res["reason"])
		}
	}
}

func TestHookIgnoresOtherSessionsFiles(t *testing.T) {
	work := fixture(t)
	// This session wrote a.go, which is committed and pushed; b.go is someone else's.
	res := hook(t, map[string]any{"transcript_path": transcript(t, filepath.Join(work, "dirty/a.go")), "cwd": work})
	if res != nil {
		t.Fatalf("want no block, got %v", res)
	}
}

func TestHookUnpushedAndNoRemote(t *testing.T) {
	work := fixture(t)
	res := hook(t, map[string]any{
		"transcript_path": transcript(t, filepath.Join(work, "ahead/a.go"), filepath.Join(work, "lonely/x.py")),
		"cwd":             work,
	})
	for _, s := range []string{"1 unpushed commit on ahead", "no remote"} {
		if !strings.Contains(res["reason"], s) {
			t.Errorf("reason lacks %q:\n%s", s, res["reason"])
		}
	}
}

func TestHookDoesNotLoop(t *testing.T) {
	work := fixture(t)
	p := map[string]any{"transcript_path": transcript(t, filepath.Join(work, "dirty/b.go")), "cwd": work, "stop_hook_active": true}
	if res := hook(t, p); res != nil {
		t.Fatalf("blocked twice: %v", res)
	}
	p["stop_hook_active"] = false
	t.Setenv("UNSAVED_HOOK", "off")
	if res := hook(t, p); res != nil {
		t.Fatalf("UNSAVED_HOOK=off still blocked: %v", res)
	}
}
