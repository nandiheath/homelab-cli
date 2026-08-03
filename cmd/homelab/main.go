package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	return router.Run(r.Args)
}

type Render struct {
	Path       string `help:"Kustomize directory to render." xor:"target"`
	All        bool   `help:"Render every direct child of --source-root." xor:"target"`
	SourceRoot string `default:"argocd/infrastructure" help:"Source root used with --all."`
	Output     string `help:"Output directory for --path."`
	OutputRoot string `default:"artifacts/infrastructure" help:"Output root used with --all."`
	Kustomize  string `default:"kustomize" help:"Kustomize executable."`
	YQ         string `default:"yq" help:"yq executable used to split resources."`
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
	if r.All {
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
		return nil
	}
	if r.Path == "" || r.Output == "" {
		return fmt.Errorf("--path requires --output")
	}
	return r.render(r.Path, r.Output)
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
