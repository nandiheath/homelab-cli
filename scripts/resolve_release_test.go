package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveNewRelease(t *testing.T) {
	repo := newRepository(t)
	commit(t, repo, "chore: initial")
	git(t, repo, "tag", "v0.1.0")
	commit(t, repo, "fix: next release")

	stdout, stderr, err := resolve(t, repo, "release", "", 1)
	if err != nil {
		t.Fatalf("resolve new release: %v\n%s", err, stderr)
	}
	if stdout != "range=v0.1.0..HEAD\ncreate_tag=true\n" {
		t.Fatalf("unexpected output:\n%s", stdout)
	}
}

func TestResolveRecovery(t *testing.T) {
	repo := newRepository(t)
	commit(t, repo, "chore: initial")
	git(t, repo, "tag", "v0.1.0")
	commit(t, repo, "fix: released")
	git(t, repo, "tag", "v0.1.1")
	commit(t, repo, "feat: later work")

	stdout, stderr, err := resolve(t, repo, "release", "v0.1.1", 1)
	if err != nil {
		t.Fatalf("resolve recovery: %v\n%s", err, stderr)
	}
	want := "tag=v0.1.1\nrange=v0.1.0..v0.1.1\ncreate_tag=false\n"
	if stdout != want {
		t.Fatalf("unexpected output:\n%s", stdout)
	}
	if head, tag := git(t, repo, "rev-parse", "HEAD"), git(t, repo, "rev-parse", "v0.1.1"); head != tag {
		t.Fatalf("recovery checked out %s, want %s", head, tag)
	}
}

func TestResolveRejectsInvalidConfirmation(t *testing.T) {
	repo := newRepository(t)
	stdout, stderr, err := resolve(t, repo, "no", "", 1)
	if err == nil {
		t.Fatalf("expected failure, got stdout %q", stdout)
	}
	if !strings.Contains(stderr, "confirm must equal release") {
		t.Fatalf("unexpected error: %s", stderr)
	}
}

func TestResolveRejectsExistingRelease(t *testing.T) {
	repo := newRepository(t)
	commit(t, repo, "chore: initial")
	git(t, repo, "tag", "v0.1.0")
	commit(t, repo, "fix: released")
	git(t, repo, "tag", "v0.1.1")

	stdout, stderr, err := resolve(t, repo, "release", "v0.1.1", 0)
	if err == nil {
		t.Fatalf("expected failure, got stdout %q", stdout)
	}
	if !strings.Contains(stderr, "release v0.1.1 already exists") {
		t.Fatalf("unexpected error: %s", stderr)
	}
}

func resolve(t *testing.T, repo, confirm, recoverTag string, ghExit int) (string, string, error) {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(filename), "resolve-release.sh")
	fakeGH := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(fakeGH, []byte("#!/bin/sh\nexit "+string(rune('0'+ghExit))+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(script)
	command.Dir = repo
	command.Env = append(os.Environ(), "CONFIRM="+confirm, "RECOVER_TAG="+recoverTag, "GH_BIN="+fakeGH)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func newRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Release Test")
	git(t, repo, "config", "user.email", "release-test@example.com")
	return repo
}

func commit(t *testing.T, repo, message string) {
	t.Helper()
	path := filepath.Join(repo, "history")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(message + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "history")
	git(t, repo, "commit", "-m", message)
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
