package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	context, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := run(context, os.Args[1:]); err != nil {
		var status *statusError
		if errors.As(err, &status) {
			fmt.Fprintln(os.Stderr, status.Error())
			os.Exit(status.Code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type statusError struct {
	Code    int
	Message string
}

func (e *statusError) Error() string { return e.Message }

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printUsage()
		return &statusError{Code: 2, Message: "a command is required"}
	}
	switch args[0] {
	case "validate":
		return validateCommand(args[1:])
	case "render":
		return renderCommand(ctx, args[1:])
	case "fingerprint":
		return fingerprintCommand(ctx, args[1:])
	case "plan":
		return planCommand(ctx, args[1:])
	case "backup":
		return backupCommand(ctx, args[1:])
	case "bootstrap-key":
		return bootstrapKeyCommand(ctx, args[1:])
	case "apply":
		return applyCommand(ctx, args[1:])
	case "firmware-verify":
		return firmwareVerifyCommand(args[1:])
	case "upgrade":
		return upgradeCommand(ctx, args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return &statusError{Code: 2, Message: "unknown command " + args[0]}
	}
}

func printUsage() {
	fmt.Print(`routerctl manages reviewed OpenWrt profiles over pinned SSH.

Usage:
  routerctl validate [--profile DIR]
  routerctl render [--profile DIR] --output DIR [--resolve-secrets]
  routerctl fingerprint --target [USER@]HOST [--port PORT]
  routerctl plan [--profile DIR] --target [USER@]HOST [--host-fingerprint SHA256:...] [--resolve-secrets]
  routerctl backup [--profile DIR] --target [USER@]HOST [--host-fingerprint SHA256:...]
  routerctl bootstrap-key [--profile DIR] --target [USER@]HOST --host-fingerprint SHA256:... --authorize INSTALL-KEY:PROFILE
  routerctl apply [--profile DIR] --target [USER@]HOST [--host-fingerprint SHA256:...] --authorize APPLY:PROFILE [--dry-run]
  routerctl firmware-verify [--profile DIR] --image FILE
  routerctl upgrade [--profile DIR] --target [USER@]HOST [--host-fingerprint SHA256:...] --image FILE --authorize UPGRADE:PROFILE [--dry-run]

Safety:
  Every profile operation requires --profile DIR.
  validate, render, fingerprint, plan, backup, firmware-verify, and --dry-run do not mutate router state.
  bootstrap-key, apply, and upgrade require exact authorization strings and a pinned host key.
  Secret resolution loads a mode-restricted token file (default ~/.config/homelab-private/op-service-account-token) only into child op processes.
`)
}

func validateCommand(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profile, err := loadProfile(*profilePath)
	if err != nil {
		return err
	}
	fmt.Printf("profile %s is valid: %d managed UCI packages, %d supported board name(s)\n", profile.Profile.Name, len(profile.Profile.ManagedPackages), len(profile.Profile.BoardNames))
	return nil
}

func renderCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("render", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	output := flags.String("output", "", "new output directory")
	resolveSecrets := flags.Bool("resolve-secrets", false, "resolve op:// references")
	opPath := flags.String("op", "op", "1Password CLI path")
	tokenFile := flags.String("service-account-token-file", defaultServiceAccountTokenFile(), "protected 1Password service-account token file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output is required")
	}
	profile, err := loadProfile(*profilePath)
	if err != nil {
		return err
	}
	if err := os.Mkdir(*output, 0700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(*output)
		}
	}()
	var resolver secretResolver
	if *resolveSecrets {
		resolver = cachedResolver(opResolver(*opPath, *tokenFile))
	}
	for _, packageName := range profile.Profile.ManagedPackages {
		rendered, err := renderPackage(ctx, profile.Packages[packageName], resolver, !*resolveSecrets)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*output, packageName), rendered, 0600); err != nil {
			return err
		}
	}
	failed = false
	mode := "review mode; secret references retained"
	if *resolveSecrets {
		mode = "deployment mode; secrets resolved into mode-0600 files"
	}
	fmt.Printf("rendered %d packages to %s (%s)\n", len(profile.Profile.ManagedPackages), *output, mode)
	return nil
}

func fingerprintCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	targetRaw := flags.String("target", "", "SSH target")
	port := flags.Int("port", 22, "SSH port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *targetRaw == "" {
		return errors.New("--target is required")
	}
	target, err := parseTarget(*targetRaw, "root", *port)
	if err != nil {
		return err
	}
	fingerprints, _, err := inspectFingerprints(ctx, target)
	if err != nil {
		return err
	}
	for _, fingerprint := range fingerprints {
		fmt.Println(fingerprint)
	}
	return nil
}

func planCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	targetRaw := flags.String("target", "", "SSH target")
	fingerprint := flags.String("host-fingerprint", "", "pinned host fingerprint override")
	resolveSecrets := flags.Bool("resolve-secrets", false, "compare 1Password-managed values")
	opPath := flags.String("op", "op", "1Password CLI path")
	tokenFile := flags.String("service-account-token-file", defaultServiceAccountTokenFile(), "protected 1Password service-account token file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profile, _, client, err := connectProfile(ctx, *profilePath, *targetRaw, *fingerprint)
	if err != nil {
		return err
	}
	defer client.close()
	board, err := client.board(ctx)
	if err != nil {
		return err
	}
	if err := requireSupportedBoard(profile, board); err != nil {
		return err
	}
	var resolver secretResolver
	if *resolveSecrets {
		resolver = cachedResolver(opResolver(*opPath, *tokenFile))
	}
	totalDrift := 0
	totalSkipped := 0
	for _, packageName := range profile.Profile.ManagedPackages {
		actualOutput, err := client.output(ctx, "uci export "+shellQuote(packageName))
		if err != nil {
			return err
		}
		actual, err := parseUCIExport(packageName, string(actualOutput))
		if err != nil {
			return err
		}
		desired, err := materializePackage(ctx, profile.Packages[packageName], resolver, !*resolveSecrets)
		if err != nil {
			return err
		}
		result := comparePackage(desired, actual, *resolveSecrets)
		for _, line := range result.Lines {
			fmt.Println(line)
		}
		totalDrift += result.Drift
		totalSkipped += result.SkippedSecrets
	}
	fmt.Printf("plan: %d drift item(s), %d managed secret comparison(s) skipped\n", totalDrift, totalSkipped)
	if totalDrift > 0 {
		return &statusError{Code: 2, Message: "configuration drift detected"}
	}
	return nil
}

func backupCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	targetRaw := flags.String("target", "", "SSH target")
	fingerprint := flags.String("host-fingerprint", "", "pinned host fingerprint override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profile, _, client, err := connectProfile(ctx, *profilePath, *targetRaw, *fingerprint)
	if err != nil {
		return err
	}
	defer client.close()
	path, err := captureBackup(ctx, profile, client)
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func bootstrapKeyCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bootstrap-key", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	targetRaw := flags.String("target", "", "SSH target")
	fingerprint := flags.String("host-fingerprint", "", "pinned host fingerprint")
	authorize := flags.String("authorize", "", "exact mutation authorization")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profile, err := loadProfile(*profilePath)
	if err != nil {
		return err
	}
	if *authorize != "INSTALL-KEY:"+profile.Profile.Name {
		return fmt.Errorf("authorization must equal INSTALL-KEY:%s", profile.Profile.Name)
	}
	if *targetRaw == "" || *fingerprint == "" {
		return errors.New("--target and --host-fingerprint are required")
	}
	target, err := parseTarget(*targetRaw, profile.Profile.SSH.User, profile.Profile.SSH.Port)
	if err != nil {
		return err
	}
	client, err := newSSHClient(ctx, target, *fingerprint)
	if err != nil {
		return err
	}
	defer client.close()
	publicKeyPath := profile.Profile.SSH.PublicKey
	if !filepath.IsAbs(publicKeyPath) {
		publicKeyPath = filepath.Join(profile.Dir, publicKeyPath)
	}
	publicKeyBytes, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return err
	}
	publicKey := strings.TrimSpace(string(publicKeyBytes))
	if !strings.HasPrefix(publicKey, "ssh-") || strings.ContainsAny(publicKey, "\r\n") {
		return errors.New("public key must be one OpenSSH public-key line")
	}
	allowedCases := make([]string, 0, len(profile.Profile.BoardNames))
	for _, board := range profile.Profile.BoardNames {
		allowedCases = append(allowedCases, shellQuote(board))
	}
	remoteCommand := "board=$(ubus call system board | jsonfilter -e '@.board_name') || exit 1; case \"$board\" in " + strings.Join(allowedCases, "|") + ") ;; *) echo \"unsupported board: $board\" >&2; exit 1 ;; esac; umask 077; mkdir -p /etc/dropbear; touch /etc/dropbear/authorized_keys; key=" + shellQuote(publicKey) + "; grep -qxF \"$key\" /etc/dropbear/authorized_keys || printf '%s\\n' \"$key\" >> /etc/dropbear/authorized_keys; chmod 600 /etc/dropbear/authorized_keys"
	if err := client.runInteractive(ctx, remoteCommand); err != nil {
		return fmt.Errorf("install public key: %w", err)
	}
	fmt.Printf("public key installed on verified %s board; password policy was not changed\n", profile.Profile.Name)
	return nil
}

func applyCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	targetRaw := flags.String("target", "", "SSH target")
	fingerprint := flags.String("host-fingerprint", "", "pinned host fingerprint override")
	authorize := flags.String("authorize", "", "exact mutation authorization")
	dryRun := flags.Bool("dry-run", false, "validate and describe without connecting")
	rollbackSeconds := flags.Int("rollback-seconds", 120, "automatic rollback delay")
	opPath := flags.String("op", "op", "1Password CLI path")
	tokenFile := flags.String("service-account-token-file", defaultServiceAccountTokenFile(), "protected 1Password service-account token file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profile, err := loadProfile(*profilePath)
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("dry-run: verify board %s; pin host key; capture local backup; resolve secrets; validate %d staged packages; arm %ds rollback; atomically replace config; reload; confirm SSH at %s\n", strings.Join(profile.Profile.BoardNames, ","), len(profile.Profile.ManagedPackages), *rollbackSeconds, profile.Profile.PostApplyHost)
		return nil
	}
	if *authorize != "APPLY:"+profile.Profile.Name {
		return fmt.Errorf("authorization must equal APPLY:%s", profile.Profile.Name)
	}
	if *rollbackSeconds < 60 || *rollbackSeconds > 600 {
		return errors.New("--rollback-seconds must be between 60 and 600")
	}
	profile, _, client, err := connectLoadedProfile(ctx, profile, *targetRaw, *fingerprint)
	if err != nil {
		return err
	}
	defer client.close()
	board, err := client.board(ctx)
	if err != nil {
		return err
	}
	if err := requireSupportedBoard(profile, board); err != nil {
		return err
	}
	backupPath, err := captureBackup(ctx, profile, client)
	if err != nil {
		return err
	}
	fmt.Println("backup:", backupPath)
	resolver := cachedResolver(opResolver(*opPath, *tokenFile))
	stageDir, err := os.MkdirTemp("", "routerctl-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)
	for _, packageName := range profile.Profile.ManagedPackages {
		rendered, err := renderPackage(ctx, profile.Packages[packageName], resolver, false)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stageDir, packageName), rendered, 0600); err != nil {
			return err
		}
	}
	transaction := "routerctl-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	remoteDir := "/tmp/" + transaction
	if err := client.run(ctx, "umask 077; mkdir -p "+remoteDir+"/stage "+remoteDir+"/backup", false); err != nil {
		return err
	}
	for _, packageName := range profile.Profile.ManagedPackages {
		if err := client.upload(ctx, filepath.Join(stageDir, packageName), remoteDir+"/stage/"+packageName); err != nil {
			return err
		}
	}
	packageWords := strings.Join(profile.Profile.ManagedPackages, " ")
	preflight := "set -eu; for f in " + packageWords + "; do uci -c " + remoteDir + "/stage show \"$f\" >/dev/null; cp -p /etc/config/\"$f\" " + remoteDir + "/backup/\"$f\"; done"
	if err := client.run(ctx, preflight, false); err != nil {
		return fmt.Errorf("remote staging validation: %w", err)
	}
	rollbackToken := remoteDir + "/ROLLBACK"
	rollback := "touch " + rollbackToken + "; (sleep " + strconv.Itoa(*rollbackSeconds) + "; if [ -e " + rollbackToken + " ]; then for f in " + packageWords + "; do cp -fp " + remoteDir + "/backup/\"$f\" /etc/config/\"$f\"; done; reload_config; fi) </dev/null >" + remoteDir + "/rollback.log 2>&1 &"
	if err := client.run(ctx, rollback, false); err != nil {
		return fmt.Errorf("arm rollback: %w", err)
	}
	install := "set -eu; for f in " + packageWords + "; do cp " + remoteDir + "/stage/\"$f\" /etc/config/\"$f\".routerctl-new; chmod 600 /etc/config/\"$f\".routerctl-new; mv -f /etc/config/\"$f\".routerctl-new /etc/config/\"$f\"; done; reload_config"
	_ = client.run(ctx, install, true)
	postTarget, err := parseTarget(profile.Profile.PostApplyHost, profile.Profile.SSH.User, profile.Profile.SSH.Port)
	if err != nil {
		return err
	}
	postClient, err := waitForSSH(ctx, profile, postTarget, selectedFingerprint(profile, *fingerprint), time.Duration(*rollbackSeconds-10)*time.Second)
	if err != nil {
		return fmt.Errorf("apply was not confirmed; timed rollback remains armed: %w", err)
	}
	defer postClient.close()
	postBoard, err := postClient.board(ctx)
	if err != nil {
		return fmt.Errorf("post-apply board check failed; timed rollback remains armed: %w", err)
	}
	if err := requireSupportedBoard(profile, postBoard); err != nil {
		return err
	}
	postApplyDrift := 0
	for _, packageName := range profile.Profile.ManagedPackages {
		actualOutput, err := postClient.output(ctx, "uci export "+shellQuote(packageName))
		if err != nil {
			return fmt.Errorf("post-apply export failed; timed rollback remains armed: %w", err)
		}
		actual, err := parseUCIExport(packageName, string(actualOutput))
		if err != nil {
			return fmt.Errorf("post-apply parse failed; timed rollback remains armed: %w", err)
		}
		desired, err := materializePackage(ctx, profile.Packages[packageName], resolver, false)
		if err != nil {
			return fmt.Errorf("post-apply secret resolution failed; timed rollback remains armed: %w", err)
		}
		postApplyDrift += comparePackage(desired, actual, true).Drift
	}
	if postApplyDrift != 0 {
		return fmt.Errorf("post-apply verification found %d drift item(s); timed rollback remains armed", postApplyDrift)
	}
	if err := postClient.run(ctx, "rm -rf "+remoteDir, false); err != nil {
		return fmt.Errorf("disarm rollback: %w", err)
	}
	fmt.Printf("applied %d packages; desired state, SSH, and board identity confirmed; rollback disarmed\n", len(profile.Profile.ManagedPackages))
	return nil
}

func firmwareVerifyCommand(args []string) error {
	flags := flag.NewFlagSet("firmware-verify", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	imagePath := flags.String("image", "", "firmware image")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *imagePath == "" {
		return errors.New("--image is required")
	}
	profile, err := loadProfile(*profilePath)
	if err != nil {
		return err
	}
	if err := verifyFirmware(profile, *imagePath, ""); err != nil {
		return err
	}
	fmt.Printf("firmware verified for %s: %s (%s)\n", profile.Profile.Name, profile.Profile.Firmware.Image, profile.Profile.Firmware.SHA256)
	return nil
}

func upgradeCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	profilePath := flags.String("profile", "", "profile directory (required)")
	targetRaw := flags.String("target", "", "SSH target")
	fingerprint := flags.String("host-fingerprint", "", "pinned host fingerprint override")
	imagePath := flags.String("image", "", "firmware image")
	authorize := flags.String("authorize", "", "exact mutation authorization")
	dryRun := flags.Bool("dry-run", false, "verify local image and describe without connecting")
	if err := flags.Parse(args); err != nil {
		return err
	}
	profile, err := loadProfile(*profilePath)
	if err != nil {
		return err
	}
	if *imagePath == "" {
		return errors.New("--image is required")
	}
	if err := verifyFirmware(profile, *imagePath, ""); err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("dry-run: verified firmware; would pin host key, verify board/target, capture backup, run sysupgrade -T, then sysupgrade with configuration retention\n")
		return nil
	}
	if *authorize != "UPGRADE:"+profile.Profile.Name {
		return fmt.Errorf("authorization must equal UPGRADE:%s", profile.Profile.Name)
	}
	profile, _, client, err := connectLoadedProfile(ctx, profile, *targetRaw, *fingerprint)
	if err != nil {
		return err
	}
	defer client.close()
	board, err := client.board(ctx)
	if err != nil {
		return err
	}
	if err := requireSupportedBoard(profile, board); err != nil {
		return err
	}
	if board.Release.Target != profile.Profile.Firmware.Target {
		return fmt.Errorf("router target %q does not match firmware target %q", board.Release.Target, profile.Profile.Firmware.Target)
	}
	backupPath, err := captureBackup(ctx, profile, client)
	if err != nil {
		return err
	}
	fmt.Println("backup:", backupPath)
	remoteImage := "/tmp/" + profile.Profile.Firmware.Image
	if err := client.upload(ctx, *imagePath, remoteImage); err != nil {
		return err
	}
	if err := client.run(ctx, "sysupgrade -T "+shellQuote(remoteImage), false); err != nil {
		return fmt.Errorf("router rejected firmware image: %w", err)
	}
	_ = client.run(ctx, "sysupgrade -v "+shellQuote(remoteImage), true)
	postTarget, err := parseTarget(profile.Profile.PostApplyHost, profile.Profile.SSH.User, profile.Profile.SSH.Port)
	if err != nil {
		return err
	}
	postClient, err := waitForSSH(ctx, profile, postTarget, selectedFingerprint(profile, *fingerprint), 5*time.Minute)
	if err != nil {
		return fmt.Errorf("router did not return after sysupgrade; use documented device recovery: %w", err)
	}
	defer postClient.close()
	postBoard, err := postClient.board(ctx)
	if err != nil {
		return err
	}
	if err := requireSupportedBoard(profile, postBoard); err != nil {
		return err
	}
	if postBoard.Release.Version != profile.Profile.Firmware.Version {
		return fmt.Errorf("router returned on version %q, expected %q", postBoard.Release.Version, profile.Profile.Firmware.Version)
	}
	fmt.Printf("router upgraded to OpenWrt %s; run plan then apply separately\n", postBoard.Release.Version)
	return nil
}

func connectProfile(ctx context.Context, profilePath, targetRaw, fingerprint string) (*LoadedProfile, targetSpec, *sshClient, error) {
	profile, err := loadProfile(profilePath)
	if err != nil {
		return nil, targetSpec{}, nil, err
	}
	return connectLoadedProfile(ctx, profile, targetRaw, fingerprint)
}

func connectLoadedProfile(ctx context.Context, profile *LoadedProfile, targetRaw, fingerprint string) (*LoadedProfile, targetSpec, *sshClient, error) {
	if targetRaw == "" {
		return nil, targetSpec{}, nil, errors.New("--target is required")
	}
	target, err := parseTarget(targetRaw, profile.Profile.SSH.User, profile.Profile.SSH.Port)
	if err != nil {
		return nil, targetSpec{}, nil, err
	}
	client, err := newSSHClient(ctx, target, selectedFingerprint(profile, fingerprint))
	if err != nil {
		return nil, targetSpec{}, nil, err
	}
	return profile, target, client, nil
}

func selectedFingerprint(profile *LoadedProfile, override string) string {
	if override != "" {
		return override
	}
	return profile.Profile.SSH.KnownHostFingerprint
}

func materializePackage(ctx context.Context, pkg UCIPackage, resolver secretResolver, redact bool) (UCIPackage, error) {
	copyPackage := UCIPackage{Package: pkg.Package, Sections: make([]UCISection, len(pkg.Sections))}
	for sectionIndex, section := range pkg.Sections {
		copySection := UCISection{Type: section.Type, Name: section.Name, Options: make(map[string]UCIValue), Lists: make(map[string][]UCIValue)}
		for key, value := range section.Options {
			if value.SecretRef != "" && !redact {
				resolved, err := resolver(ctx, value.SecretRef)
				if err != nil {
					return UCIPackage{}, err
				}
				copySection.Options[key] = UCIValue{Literal: &resolved}
			} else {
				copySection.Options[key] = value
			}
		}
		for key, values := range section.Lists {
			copySection.Lists[key] = make([]UCIValue, len(values))
			for valueIndex, value := range values {
				if value.SecretRef != "" && !redact {
					resolved, err := resolver(ctx, value.SecretRef)
					if err != nil {
						return UCIPackage{}, err
					}
					copySection.Lists[key][valueIndex] = UCIValue{Literal: &resolved}
				} else {
					copySection.Lists[key][valueIndex] = value
				}
			}
		}
		copyPackage.Sections[sectionIndex] = copySection
	}
	return copyPackage, nil
}

func cachedResolver(resolver secretResolver) secretResolver {
	cache := make(map[string]string)
	return func(ctx context.Context, reference string) (string, error) {
		if value, ok := cache[reference]; ok {
			return value, nil
		}
		value, err := resolver(ctx, reference)
		if err != nil {
			return "", err
		}
		cache[reference] = value
		return value, nil
	}
}

func captureBackup(ctx context.Context, profile *LoadedProfile, client *sshClient) (string, error) {
	board, err := client.board(ctx)
	if err != nil {
		return "", err
	}
	if err := requireSupportedBoard(profile, board); err != nil {
		return "", err
	}
	repositoryRoot, err := findRepositoryRoot(profile.Dir)
	if err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	path := filepath.Join(repositoryRoot, "backups", profile.Profile.Name+"-"+stamp)
	if err := os.Mkdir(path, 0700); err != nil {
		return "", err
	}
	failed := true
	defer func() {
		if failed {
			_ = os.RemoveAll(path)
		}
	}()
	boardJSON, err := json.MarshalIndent(board, "", "  ")
	if err != nil {
		return "", err
	}
	boardJSON = append(boardJSON, '\n')
	if err := os.WriteFile(filepath.Join(path, "board.json"), boardJSON, 0600); err != nil {
		return "", err
	}
	for _, packageName := range profile.Profile.ManagedPackages {
		output, err := client.output(ctx, "uci export "+shellQuote(packageName))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(path, packageName+".uci"), output, 0600); err != nil {
			return "", err
		}
	}
	for fileName, command := range map[string]string{
		"installed-packages.txt": "opkg list-installed",
		"sysupgrade-files.txt":   "sysupgrade -l",
		"uci-changes.txt":        "uci changes",
	} {
		output, err := client.output(ctx, command)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(path, fileName), output, 0600); err != nil {
			return "", err
		}
	}
	archive, err := client.output(ctx, "sysupgrade -b -")
	if err != nil {
		return "", err
	}
	if len(archive) < 2 || archive[0] != 0x1f || archive[1] != 0x8b {
		return "", errors.New("sysupgrade backup did not return a gzip archive")
	}
	if err := os.WriteFile(filepath.Join(path, "sysupgrade-backup.tar.gz"), archive, 0600); err != nil {
		return "", err
	}
	failed = false
	return path, nil
}

func findRepositoryRoot(start string) (string, error) {
	current := start
	for {
		if info, err := os.Stat(filepath.Join(current, ".git")); err == nil && info.IsDir() {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("profile is not inside a Git working tree")
		}
		current = parent
	}
}
