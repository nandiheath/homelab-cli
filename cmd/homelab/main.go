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
	Render Render `cmd:"" help:"Render Helm or Kustomize sources into Kubernetes manifest files."`
}

type Router struct {
	Args []string `arg:"" passthrough:""`
}

func (r Router) Run() error {
	return router.Run(r.Args, version)
}

type Render struct {
	Path          string `help:"Helm or Kustomize source directory to render." xor:"target"`
	All           bool   `help:"Render every source discovered recursively below --source-root." xor:"target"`
	CI            bool   `help:"Render source directories changed by the current CI event." xor:"target"`
	CommitAndPush bool   `help:"Commit artifact-only changes and push them to the current branch."`
	FailOnChange  bool   `help:"Return a failure after changed artifacts are committed and pushed."`
	SourceRoot    string `default:"argocd" help:"Source root used with --all and --ci."`
	Output        string `help:"Output directory for --path."`
	OutputRoot    string `default:"artifacts" help:"Output root used with --all and --ci."`
	Helm          string `default:"helm" help:"Helm executable."`
	Kustomize     string `default:"kustomize" help:"Kustomize executable."`
	YQ            string `default:"yq" help:"yq executable used to split resources."`
	Git           string `default:"git" help:"Git executable used by CI mode."`
}

type renderEngine string

const (
	engineHelm      renderEngine = "helm"
	engineKustomize renderEngine = "kustomize"
)

type sourceUnit struct {
	Source   string
	Relative string
	Engine   renderEngine
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
	if r.FailOnChange && !r.CommitAndPush {
		return fmt.Errorf("--fail-on-change requires --commit-and-push")
	}
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
		if err := r.renderAll(); err != nil {
			return err
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
		changed, err := r.commitAndPush()
		if err != nil {
			return err
		}
		if changed && r.FailOnChange {
			return fmt.Errorf("rendered artifacts changed and were committed")
		}
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
	changedPaths := make([]string, 0)
	for _, changedPath := range bytes.Split(changed, []byte{0}) {
		if len(changedPath) == 0 {
			continue
		}
		relative, err := filepath.Rel(filepath.Clean(r.SourceRoot), filepath.FromSlash(string(changedPath)))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("map changed source %s", changedPath)
		}
		changedPaths = append(changedPaths, relative)
	}
	if len(changedPaths) == 0 {
		fmt.Println("No changed manifest sources.")
		return nil
	}

	units, err := discoverSourceUnits(r.SourceRoot)
	if err != nil {
		return err
	}
	current := make(map[string]sourceUnit, len(units))
	affected := map[string]sourceUnit{}
	for _, unit := range units {
		current[unit.Relative] = unit
		for _, changedPath := range changedPaths {
			if pathWithin(unit.Relative, changedPath) {
				affected[unit.Relative] = unit
				break
			}
		}
	}
	outputs, err := discoverOutputUnits(r.OutputRoot)
	if err != nil {
		return err
	}
	deleted := map[string]struct{}{}
	for _, output := range outputs {
		if _, exists := current[output]; exists {
			continue
		}
		for _, changedPath := range changedPaths {
			if pathWithin(output, changedPath) {
				deleted[output] = struct{}{}
				break
			}
		}
	}

	paths := make([]string, 0, len(affected))
	for path := range affected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		unit := affected[path]
		if err := r.render(unit.Source, filepath.Join(r.OutputRoot, unit.Relative)); err != nil {
			return err
		}
	}
	deletedPaths := make([]string, 0, len(deleted))
	for path := range deleted {
		deletedPaths = append(deletedPaths, path)
	}
	sort.Strings(deletedPaths)
	for _, path := range deletedPaths {
		output := filepath.Join(r.OutputRoot, path)
		if err := os.RemoveAll(output); err != nil {
			return fmt.Errorf("remove deleted source output %s: %w", output, err)
		}
		removeEmptyParents(filepath.Dir(output), filepath.Clean(r.OutputRoot))
	}
	if len(paths) == 0 && len(deletedPaths) == 0 {
		fmt.Println("No changed manifest sources.")
	}
	return nil
}

func (r *Render) commitAndPush() (bool, error) {
	status, err := r.gitOutput("status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	paths, err := parseStatusPaths(status)
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		fmt.Println("Rendered artifacts are current.")
		return false, nil
	}
	for _, path := range paths {
		if !pathWithin(r.OutputRoot, path) {
			return false, fmt.Errorf("refusing to commit non-artifact change %s", path)
		}
	}
	for _, args := range [][]string{
		{"config", "user.name", "github-actions[bot]"},
		{"config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com"},
		{"add", "--all", "--", filepath.ToSlash(filepath.Clean(r.OutputRoot))},
		{"commit", "-m", "chore(artifacts): render changed manifests"},
	} {
		if _, err := r.gitOutput(args...); err != nil {
			return false, err
		}
	}
	_, target, err := ciGitContext()
	if err != nil {
		return false, err
	}
	if target == "" {
		branch, err := r.gitOutput("branch", "--show-current")
		if err != nil {
			return false, err
		}
		target = strings.TrimSpace(string(branch))
	}
	if target == "" {
		return false, fmt.Errorf("cannot determine branch for artifact push")
	}
	if _, err = r.gitOutput("push", "origin", "HEAD:"+target); err != nil {
		return false, err
	}
	return true, nil
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

func discoverSourceUnits(root string) ([]sourceUnit, error) {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	units := make([]sourceUnit, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		engine, err := detectRenderEngine(path)
		if err != nil {
			return err
		}
		if engine == "" {
			return nil
		}
		relative, err := filepath.Rel(filepath.Clean(root), path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("map source path %s below %s", path, root)
		}
		units = append(units, sourceUnit{Source: path, Relative: relative, Engine: engine})
		return filepath.SkipDir
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(units, func(i, j int) bool {
		return filepath.ToSlash(units[i].Relative) < filepath.ToSlash(units[j].Relative)
	})
	return units, nil
}

func detectRenderEngine(source string) (renderEngine, error) {
	chart, err := fileExists(filepath.Join(source, "Chart.yaml"))
	if err != nil {
		return "", err
	}
	values, err := fileExists(filepath.Join(source, "values.yaml"))
	if err != nil {
		return "", err
	}
	kustomization, err := fileExists(filepath.Join(source, "kustomization.yaml"))
	if err != nil {
		return "", err
	}
	if chart && kustomization {
		return "", fmt.Errorf("source %s is ambiguous: Chart.yaml and kustomization.yaml are both present", source)
	}
	if chart && !values {
		return "", fmt.Errorf("Helm source %s requires values.yaml", source)
	}
	if values && !chart && !kustomization {
		return "", fmt.Errorf("source %s has values.yaml without Chart.yaml or kustomization.yaml", source)
	}
	switch {
	case chart:
		return engineHelm, nil
	case kustomization:
		return engineKustomize, nil
	default:
		return "", nil
	}
}

func fileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.Mode().IsRegular():
		return true, nil
	case err == nil:
		return false, fmt.Errorf("source marker %s is not a regular file", path)
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

func discoverOutputUnits(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var units []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() || path == root {
			return nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, child := range entries {
			extension := strings.ToLower(filepath.Ext(child.Name()))
			if !child.IsDir() && (extension == ".yml" || extension == ".yaml") {
				relative, err := filepath.Rel(filepath.Clean(root), path)
				if err != nil {
					return err
				}
				units = append(units, relative)
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(units)
	return units, nil
}

func removeEmptyParents(path, stop string) {
	for path != stop && pathWithin(stop, path) {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			return
		}
		if err := os.Remove(path); err != nil {
			return
		}
		path = filepath.Dir(path)
	}
}

func (r *Render) renderAll() error {
	units, err := discoverSourceUnits(r.SourceRoot)
	if err != nil {
		return err
	}
	outputRoot := filepath.Clean(r.OutputRoot)
	if err := os.MkdirAll(filepath.Dir(outputRoot), 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(outputRoot), "."+filepath.Base(outputRoot)+".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	for _, unit := range units {
		if err := r.render(unit.Source, filepath.Join(staging, unit.Relative)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(outputRoot); err != nil {
		return err
	}
	return os.Rename(staging, outputRoot)
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.FromSlash(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (r *Render) render(source, output string) error {
	engine, err := detectRenderEngine(source)
	if err != nil {
		return err
	}
	if engine == "" {
		return fmt.Errorf("source %s has no Chart.yaml or kustomization.yaml", source)
	}
	tmp, err := os.MkdirTemp("", "homelab-render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyAndInterpolate(source, tmp); err != nil {
		return err
	}

	var manifest []byte
	switch engine {
	case engineKustomize:
		build := exec.Command(r.Kustomize, "build", "--enable-helm", tmp)
		manifest, err = build.CombinedOutput()
		if err != nil {
			return fmt.Errorf("kustomize build %s: %s: %w", source, strings.TrimSpace(string(manifest)), err)
		}
	case engineHelm:
		dependency := exec.Command(r.Helm, "dependency", "build", tmp)
		if output, dependencyErr := dependency.CombinedOutput(); dependencyErr != nil {
			return fmt.Errorf("helm dependency build %s: %s: %w", source, strings.TrimSpace(string(output)), dependencyErr)
		}
		releaseName := strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(filepath.Base(filepath.Clean(source))))
		build := exec.Command(r.Helm, "template", releaseName, tmp, "--values", filepath.Join(tmp, "values.yaml"), "--include-crds")
		manifest, err = build.CombinedOutput()
		if err != nil {
			return fmt.Errorf("helm template %s: %s: %w", source, strings.TrimSpace(string(manifest)), err)
		}
	default:
		return fmt.Errorf("unsupported render engine %q", engine)
	}

	if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
		return err
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
