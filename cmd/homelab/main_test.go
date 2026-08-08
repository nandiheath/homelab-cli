package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderCIAndCommitAndPush(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "clone", remote, work)
	runGit(t, work, "config", "user.name", "Render Test")
	runGit(t, work, "config", "user.email", "render@example.invalid")

	writeTestFile(t, filepath.Join(work, "README.md"), "fixture\n", 0o644)
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "initial")
	runGit(t, work, "branch", "-M", "feature")
	runGit(t, work, "push", "-u", "origin", "feature")
	base := strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))

	source := filepath.Join(work, "argocd", "infrastructure", "alpha")
	writeTestFile(t, filepath.Join(source, "kustomization.yaml"), "resources:\n- configmap.yaml\n", 0o644)
	writeTestFile(t, filepath.Join(source, "configmap.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: alpha\n", 0o644)
	runGit(t, work, "add", "argocd")
	runGit(t, work, "commit", "-m", "add source")

	kustomize := filepath.Join(root, "kustomize")
	writeTestFile(t, kustomize, "#!/bin/sh\nprintf '%s\\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: alpha'\n", 0o755)
	yq := filepath.Join(root, "yq")
	writeTestFile(t, yq, "#!/bin/sh\nprefix=${2#\\\"}\nprefix=${prefix%%\\\"*}\nmkdir -p \"$prefix\"\ncat > \"${prefix}configmap_alpha.yml\"\n", 0o755)
	eventPath := filepath.Join(root, "event.json")
	writeEvent(t, eventPath, base, "feature")
	t.Setenv("GITHUB_EVENT_PATH", eventPath)
	t.Setenv("GITHUB_HEAD_REF", "feature")

	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })

	render := Render{
		CI:         true,
		SourceRoot: "argocd",
		OutputRoot: "artifacts",
		Kustomize:  kustomize,
		YQ:         yq,
		Git:        "git",
	}
	if err := render.Run(); err != nil {
		t.Fatalf("render changed source: %v", err)
	}
	artifact := filepath.Join(work, "artifacts", "infrastructure", "alpha", "configmap_alpha.yml")
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("expected rendered artifact: %v", err)
	}

	commit := Render{CommitAndPush: true, OutputRoot: "artifacts", Git: "git"}
	if err := commit.Run(); err != nil {
		t.Fatalf("commit and push artifacts: %v", err)
	}
	localHead := strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))
	remoteHead := strings.Fields(runGit(t, work, "ls-remote", "origin", "refs/heads/feature"))[0]
	if localHead != remoteHead {
		t.Fatalf("remote head %s does not match local head %s", remoteHead, localHead)
	}
	if subject := strings.TrimSpace(runGit(t, work, "log", "-1", "--pretty=%s")); subject != "chore(artifacts): render changed manifests" {
		t.Fatalf("unexpected artifact commit subject %q", subject)
	}

	writeEvent(t, eventPath, localHead, "feature")
	if err := render.Run(); err != nil {
		t.Fatalf("no-change render: %v", err)
	}
	if err := commit.Run(); err != nil {
		t.Fatalf("no-change commit: %v", err)
	}
	if head := strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD")); head != localHead {
		t.Fatalf("no-change run created commit %s", head)
	}

	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "--all", "argocd")
	runGit(t, work, "commit", "-m", "delete source")
	writeEvent(t, eventPath, localHead, "feature")
	if err := render.Run(); err != nil {
		t.Fatalf("render deleted source: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(artifact)); !os.IsNotExist(err) {
		t.Fatalf("expected deleted source output to be absent, got %v", err)
	}
	if err := commit.Run(); err != nil {
		t.Fatalf("commit deleted source output: %v", err)
	}

	writeTestFile(t, filepath.Join(work, "artifacts", "drift.yml"), "generated\n", 0o644)
	failAfterPush := Render{CommitAndPush: true, FailOnChange: true, OutputRoot: "artifacts", Git: "git"}
	err = failAfterPush.Run()
	if err == nil || !strings.Contains(err.Error(), "changed and were committed") {
		t.Fatalf("expected committed drift failure, got %v", err)
	}
	localHead = strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))
	remoteHead = strings.Fields(runGit(t, work, "ls-remote", "origin", "refs/heads/feature"))[0]
	if localHead != remoteHead {
		t.Fatalf("fail-on-change did not push remote head %s, local %s", remoteHead, localHead)
	}
	writeTestFile(t, filepath.Join(work, "unexpected.txt"), "dirty\n", 0o644)
	if err := commit.Run(); err == nil || !strings.Contains(err.Error(), "refusing to commit non-artifact change") {
		t.Fatalf("expected non-artifact rejection, got %v", err)
	}
}

func TestRenderDiscoversHelmAndKustomizeSources(t *testing.T) {
	root := t.TempDir()
	sources := filepath.Join(root, "argocd")
	outputs := filepath.Join(root, "artifacts")

	helmSource := filepath.Join(sources, "application", "helm-app")
	writeTestFile(t, filepath.Join(helmSource, "Chart.yaml"), "apiVersion: v2\nname: helm-app\nversion: 1.0.0\n", 0o644)
	writeTestFile(t, filepath.Join(helmSource, "values.yaml"), "enabled: true\n", 0o644)
	kustomizeSource := filepath.Join(sources, "infrastructure", "kustom-app")
	writeTestFile(t, filepath.Join(kustomizeSource, "kustomization.yaml"), "resources:\n- configmap.yaml\n", 0o644)
	writeTestFile(t, filepath.Join(kustomizeSource, "configmap.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kustom-app\n", 0o644)
	writeTestFile(t, filepath.Join(outputs, "stale", "configmap_stale.yml"), "stale\n", 0o644)

	helm := filepath.Join(root, "helm")
	writeTestFile(t, helm, "#!/bin/sh\ncase \"$1\" in\ndependency) exit 0 ;;\ntemplate) printf '%s\\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: helm-app' ;;\n*) exit 2 ;;\nesac\n", 0o755)
	kustomize := filepath.Join(root, "kustomize")
	writeTestFile(t, kustomize, "#!/bin/sh\nprintf '%s\\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: kustom-app'\n", 0o755)
	yq := filepath.Join(root, "yq")
	writeTestFile(t, yq, "#!/bin/sh\nprefix=${2#\\\"}\nprefix=${prefix%%\\\"*}\nmkdir -p \"$prefix\"\nname=$(sed -n 's/^  name: //p')\nprintf '%s\\n' rendered > \"${prefix}configmap_${name}.yml\"\n", 0o755)

	render := Render{
		All:        true,
		SourceRoot: sources,
		OutputRoot: outputs,
		Helm:       helm,
		Kustomize:  kustomize,
		YQ:         yq,
	}
	if err := render.Run(); err != nil {
		t.Fatalf("render discovered sources: %v", err)
	}
	for _, path := range []string{
		filepath.Join(outputs, "application", "helm-app", "configmap_helm-app.yml"),
		filepath.Join(outputs, "infrastructure", "kustom-app", "configmap_kustom-app.yml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected rendered output %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outputs, "stale")); !os.IsNotExist(err) {
		t.Fatalf("expected full render to remove stale output, got %v", err)
	}

	writeTestFile(t, filepath.Join(kustomizeSource, "Chart.yaml"), "apiVersion: v2\nname: ambiguous\nversion: 1.0.0\n", 0o644)
	writeTestFile(t, filepath.Join(kustomizeSource, "values.yaml"), "{}\n", 0o644)
	if err := render.Run(); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous source rejection, got %v", err)
	}
	if err := os.Remove(filepath.Join(kustomizeSource, "Chart.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(kustomizeSource, "values.yaml")); err != nil {
		t.Fatal(err)
	}

	incomplete := filepath.Join(sources, "application", "incomplete")
	writeTestFile(t, filepath.Join(incomplete, "Chart.yaml"), "apiVersion: v2\nname: incomplete\nversion: 1.0.0\n", 0o644)
	if _, err := discoverSourceUnits(sources); err == nil || !strings.Contains(err.Error(), "requires values.yaml") {
		t.Fatalf("expected incomplete Helm source rejection, got %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), output, err)
	}
	return string(output)
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func writeEvent(t *testing.T, path, base, head string) {
	t.Helper()
	event := githubEvent{}
	event.PullRequest = &struct {
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}{}
	event.PullRequest.Base.SHA = base
	event.PullRequest.Head.Ref = head
	content, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, string(content), 0o644)
}
