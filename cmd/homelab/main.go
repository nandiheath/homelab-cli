package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/nandiheath/homelab-cli/internal/router"
)

var version = "dev"

type CLI struct {
	Argocd  Argocd  `cmd:"" help:"Manage Argo CD source rendering."`
	Router  Router  `cmd:"" help:"Manage reviewed OpenWrt router profiles."`
	Version Version `cmd:"" help:"Print the homelab CLI version."`
}

type Argocd struct {
	Render Render `cmd:"" help:"Render Kustomize sources into Kubernetes manifest files."`
}

type Router struct {
	Args []string `arg:"" passthrough:""`
}

func (r Router) Run() error {
	return router.Run(r.Args, version)
}

type Render struct {
	Path          string `help:"Kustomize directory to render." xor:"target"`
	All           bool   `help:"Render every direct child of --source-root." xor:"target"`
	CI            bool   `help:"Render source directories changed by the current CI event." xor:"target"`
	CommitAndPush bool   `help:"Commit artifact-only changes and push them to the current pull-request branch."`
	SourceRoot    string `default:"argocd/infrastructure" help:"Source root used with --all and --ci."`
	Output        string `help:"Output directory for --path."`
	OutputRoot    string `default:"artifacts/infrastructure" help:"Output root used with --all and --ci."`
	Kustomize     string `default:"kustomize" help:"Kustomize executable."`
	YQ            string `default:"yq" help:"yq executable used to split resources."`
	Git           string `default:"git" help:"Git executable used by CI mode."`
}

type Version struct{}

func (Version) Run() error {
	fmt.Println(version)
	return nil
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli, kong.Name("homelab"), kong.Description("Homelab operations CLI."))
	if err := ctx.Run(); err != nil {
		ctx.FatalIfErrorf(err)
	}
}

func (r *Render) Run() error {
	switch {
	case r.CI:
		if r.Output != "" {
			return fmt.Errorf("--output cannot be used with --ci; use --output-root")
		}
		if err := r.renderChangedSources(); err != nil {
			return err
		}
	case r.All:
		if r.Output != "" {
			return fmt.Errorf("--output cannot be used with --all; use --output-root")
		}
		entries, err := os.ReadDir(r.SourceRoot)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if err := r.render(filepath.Join(r.SourceRoot, entry.Name()), filepath.Join(r.OutputRoot, entry.Name())); err != nil {
				return err
			}
		}
	case r.Path != "":
		if r.Output == "" {
			return fmt.Errorf("--path requires --output")
		}
		if err := r.render(r.Path, r.Output); err != nil {
			return err
		}
	case !r.CommitAndPush:
		return fmt.Errorf("one of --path, --all, --ci, or --commit-and-push is required")
	}
	if r.CommitAndPush {
		return r.commitAndPush()
	}
	return nil
}

type githubEvent struct {
	Before      string `json:"before"`
	PullRequest *struct {
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
}

func (r *Render) renderChangedSources() error {
	status, err := r.gitOutput("status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if len(status) != 0 {
		return fmt.Errorf("repository is not clean before CI rendering")
	}
	base, _, err := ciGitContext()
	if err != nil {
		return err
	}
	if err := exec.Command(r.Git, "cat-file", "-e", base+"^{commit}").Run(); err != nil {
		if _, fetchErr := r.gitOutput("fetch", "--no-tags", "--depth=1", "origin", base); fetchErr != nil {
			return fmt.Errorf("fetch CI base %s: %w", base, fetchErr)
		}
	}
	changed, err := r.gitOutput(
		"diff", "--name-only", "--no-renames", "-z",
		base, "HEAD", "--", filepath.ToSlash(filepath.Clean(r.SourceRoot)),
	)
	if err != nil {
		return err
	}
	sourceNames := map[string]struct{}{}
	for _, changedPath := range bytes.Split(changed, []byte{0}) {
		if len(changedPath) == 0 {
			continue
		}
		relative, err := filepath.Rel(filepath.Clean(r.SourceRoot), filepath.FromSlash(string(changedPath)))
		if err != nil {
			return fmt.Errorf("map changed source %s: %w", changedPath, err)
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) < 2 || parts[0] == "." || parts[0] == ".." {
			continue
		}
		sourceNames[parts[0]] = struct{}{}
	}
	if len(sourceNames) == 0 {
		fmt.Println("No changed manifest sources.")
		return nil
	}
	names := make([]string, 0, len(sourceNames))
	for name := range sourceNames {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		source := filepath.Join(r.SourceRoot, name)
		output := filepath.Join(r.OutputRoot, name)
		_, err := os.Stat(filepath.Join(source, "kustomization.yaml"))
		switch {
		case err == nil:
			if err := r.render(source, output); err != nil {
				return err
			}
		case os.IsNotExist(err):
			if err := os.RemoveAll(output); err != nil {
				return fmt.Errorf("remove deleted source output %s: %w", output, err)
			}
		default:
			return fmt.Errorf("inspect changed source %s: %w", source, err)
		}
	}
	return nil
}

func (r *Render) commitAndPush() error {
	status, err := r.gitOutput("status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	paths, err := parseStatusPaths(status)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		fmt.Println("Rendered artifacts are current.")
		return nil
	}
	for _, path := range paths {
		if !pathWithin(r.OutputRoot, path) {
			return fmt.Errorf("refusing to commit non-artifact change %s", path)
		}
	}
	for _, args := range [][]string{
		{"config", "user.name", "github-actions[bot]"},
		{"config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com"},
		{"add", "--all", "--", filepath.ToSlash(filepath.Clean(r.OutputRoot))},
		{"commit", "-m", "chore(artifacts): render changed manifests"},
	} {
		if _, err := r.gitOutput(args...); err != nil {
			return err
		}
	}
	_, target, err := ciGitContext()
	if err != nil {
		return err
	}
	if target == "" {
		branch, err := r.gitOutput("branch", "--show-current")
		if err != nil {
			return err
		}
		target = strings.TrimSpace(string(branch))
	}
	if target == "" {
		return fmt.Errorf("cannot determine branch for artifact push")
	}
	_, err = r.gitOutput("push", "origin", "HEAD:"+target)
	return err
}

func (r *Render) gitOutput(args ...string) ([]byte, error) {
	command := exec.Command(r.Git, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(output)), err)
	}
	return output, nil
}

func ciGitContext() (base, target string, err error) {
	var event githubEvent
	if eventPath := os.Getenv("GITHUB_EVENT_PATH"); eventPath != "" {
		content, readErr := os.ReadFile(eventPath)
		if readErr != nil {
			return "", "", fmt.Errorf("read GitHub event: %w", readErr)
		}
		if unmarshalErr := json.Unmarshal(content, &event); unmarshalErr != nil {
			return "", "", fmt.Errorf("parse GitHub event: %w", unmarshalErr)
		}
	}
	base = os.Getenv("GITHUB_EVENT_BEFORE")
	if base == "" {
		base = event.Before
	}
	if isZeroSHA(base) {
		base = ""
	}
	if base == "" && event.PullRequest != nil {
		base = event.PullRequest.Base.SHA
	}
	if base == "" {
		base = "HEAD^"
	}
	target = os.Getenv("GITHUB_HEAD_REF")
	if target == "" && event.PullRequest != nil {
		target = event.PullRequest.Head.Ref
	}
	if target == "" {
		target = os.Getenv("GITHUB_REF_NAME")
	}
	return base, target, nil
}

func isZeroSHA(value string) bool {
	return value != "" && strings.Trim(value, "0") == ""
}

func parseStatusPaths(status []byte) ([]string, error) {
	records := bytes.Split(status, []byte{0})
	paths := make([]string, 0, len(records))
	for index := 0; index < len(records); index++ {
		record := records[index]
		if len(record) == 0 {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("parse git status record %q", record)
		}
		paths = append(paths, string(record[3:]))
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			index++
			if index >= len(records) || len(records[index]) == 0 {
				return nil, fmt.Errorf("parse renamed git status record %q", record)
			}
			paths = append(paths, string(records[index]))
		}
	}
	return paths, nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.FromSlash(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (r *Render) render(source, output string) error {
	if _, err := os.Stat(filepath.Join(source, "kustomization.yaml")); err != nil {
		return fmt.Errorf("validate source %s: %w", source, err)
	}
	tmp, err := os.MkdirTemp("", "homelab-render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyAndInterpolate(source, tmp); err != nil {
		return err
	}
	build := exec.Command(r.Kustomize, "build", "--enable-helm", tmp)
	manifest, err := build.Output()
	if err != nil {
		return fmt.Errorf("kustomize build %s: %w", source, err)
	}
	staging := output + ".staging"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0755); err != nil {
		return err
	}
	yq := exec.Command(r.YQ, "-s", `"`+staging+`/" + (.kind | downcase) + "_" + (.metadata.name | sub("\\.", "-"))`)
	yq.Stdin = bytes.NewReader(manifest)
	if output, err := yq.CombinedOutput(); err != nil {
		return fmt.Errorf("split rendered manifests: %s: %w", strings.TrimSpace(string(output)), err)
	}
	if err := os.RemoveAll(output); err != nil {
		return err
	}
	return os.Rename(staging, output)
}

func copyAndInterpolate(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, name := range []string{"ARGOCD_GITHUB_REPO", "ARGOCD_GITHUB_ORG", "VAULT", "ARGOCD_ADMIN_GITHUB_USER"} {
			value := os.Getenv(name)
			if value == "" {
				value = map[string]string{"ARGOCD_GITHUB_REPO": "https://github.com/nandiheath/homelab-public.git", "ARGOCD_GITHUB_ORG": "https://github.com/nandiheath", "VAULT": "homelab", "ARGOCD_ADMIN_GITHUB_USER": "nandiheath"}[name]
			}
			content = []byte(strings.ReplaceAll(strings.ReplaceAll(string(content), "${"+name+"}", value), "$"+name, value))
		}
		return os.WriteFile(target, content, 0644)
	})
}
