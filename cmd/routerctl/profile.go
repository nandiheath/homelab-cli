package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var identifierRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
var sha256RE = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Profile struct {
	SchemaVersion   int      `json:"schema_version"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	BoardNames      []string `json:"board_names"`
	ManagedPackages []string `json:"managed_packages"`
	PostApplyHost   string   `json:"post_apply_host"`
	SSH             SSHSpec  `json:"ssh"`
	Firmware        Firmware `json:"firmware"`
}

type SSHSpec struct {
	User                 string `json:"user"`
	Port                 int    `json:"port"`
	PublicKey            string `json:"public_key"`
	KnownHostFingerprint string `json:"known_host_fingerprint"`
}

type Firmware struct {
	Version string `json:"version"`
	Target  string `json:"target"`
	Image   string `json:"image"`
	SHA256  string `json:"sha256"`
}

type UCIPackage struct {
	Package  string       `json:"package"`
	Sections []UCISection `json:"sections"`
}

type UCISection struct {
	Type    string                `json:"type"`
	Name    string                `json:"name,omitempty"`
	Options map[string]UCIValue   `json:"options,omitempty"`
	Lists   map[string][]UCIValue `json:"lists,omitempty"`
}

type UCIValue struct {
	Literal   *string
	SecretRef string
}

func (v *UCIValue) UnmarshalJSON(data []byte) error {
	var literal string
	if err := json.Unmarshal(data, &literal); err == nil {
		v.Literal = &literal
		return nil
	}
	var secret struct {
		SecretRef string `json:"secret_ref"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&secret); err != nil {
		return errors.New("value must be a string or {\"secret_ref\":\"op://...\"}")
	}
	if secret.SecretRef == "" {
		return errors.New("secret_ref must not be empty")
	}
	v.SecretRef = secret.SecretRef
	return nil
}

func (v UCIValue) MarshalJSON() ([]byte, error) {
	if v.SecretRef != "" {
		return json.Marshal(struct {
			SecretRef string `json:"secret_ref"`
		}{v.SecretRef})
	}
	if v.Literal == nil {
		return nil, errors.New("UCI value has neither literal nor secret_ref")
	}
	return json.Marshal(*v.Literal)
}

type LoadedProfile struct {
	Dir      string
	Profile  Profile
	Packages map[string]UCIPackage
}

func loadProfile(dir string) (*LoadedProfile, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var profile Profile
	if err := decodeJSONFile(filepath.Join(absolute, "profile.json"), &profile); err != nil {
		return nil, err
	}
	loaded := &LoadedProfile{Dir: absolute, Profile: profile, Packages: make(map[string]UCIPackage)}
	for _, name := range profile.ManagedPackages {
		var pkg UCIPackage
		if err := decodeJSONFile(filepath.Join(absolute, "uci", name+".json"), &pkg); err != nil {
			return nil, err
		}
		loaded.Packages[name] = pkg
	}
	if err := loaded.validate(); err != nil {
		return nil, err
	}
	return loaded, nil
}

func decodeJSONFile(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	if err := dec.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (p *LoadedProfile) validate() error {
	var problems []string
	profile := p.Profile
	if profile.SchemaVersion != 1 {
		problems = append(problems, "schema_version must be 1")
	}
	if !identifierRE.MatchString(profile.Name) {
		problems = append(problems, "name must contain only letters, numbers, underscore, and hyphen")
	}
	if len(profile.BoardNames) == 0 {
		problems = append(problems, "board_names must not be empty")
	}
	if len(profile.ManagedPackages) == 0 {
		problems = append(problems, "managed_packages must not be empty")
	}
	if profile.PostApplyHost == "" {
		problems = append(problems, "post_apply_host must not be empty")
	}
	if !identifierRE.MatchString(profile.SSH.User) {
		problems = append(problems, "ssh.user is invalid")
	}
	if profile.SSH.Port < 1 || profile.SSH.Port > 65535 {
		problems = append(problems, "ssh.port must be between 1 and 65535")
	}
	if profile.SSH.PublicKey == "" {
		problems = append(problems, "ssh.public_key must not be empty")
	}
	if !strings.HasPrefix(profile.SSH.KnownHostFingerprint, "SHA256:") {
		problems = append(problems, "ssh.known_host_fingerprint must be an SHA256 fingerprint")
	}
	if profile.Firmware.Version == "" || profile.Firmware.Target == "" || profile.Firmware.Image == "" {
		problems = append(problems, "firmware version, target, and image are required")
	}
	if !sha256RE.MatchString(profile.Firmware.SHA256) {
		problems = append(problems, "firmware.sha256 must be 64 lowercase hexadecimal characters")
	}
	seenPackages := make(map[string]bool)
	for _, packageName := range profile.ManagedPackages {
		if !identifierRE.MatchString(packageName) {
			problems = append(problems, fmt.Sprintf("managed package %q is invalid", packageName))
			continue
		}
		if seenPackages[packageName] {
			problems = append(problems, fmt.Sprintf("managed package %q is duplicated", packageName))
		}
		seenPackages[packageName] = true
		pkg, ok := p.Packages[packageName]
		if !ok {
			problems = append(problems, fmt.Sprintf("managed package %q was not loaded", packageName))
			continue
		}
		if pkg.Package != packageName {
			problems = append(problems, fmt.Sprintf("uci/%s.json declares package %q", packageName, pkg.Package))
		}
		for sectionIndex, section := range pkg.Sections {
			location := fmt.Sprintf("%s section %d", packageName, sectionIndex+1)
			if !identifierRE.MatchString(section.Type) {
				problems = append(problems, location+" has invalid type")
			}
			if section.Name != "" && !identifierRE.MatchString(section.Name) {
				problems = append(problems, location+" has invalid name")
			}
			for key, value := range section.Options {
				problems = append(problems, validateValue(packageName, section, "option", key, value)...)
			}
			for key, values := range section.Lists {
				if len(values) == 0 {
					problems = append(problems, fmt.Sprintf("%s list %q is empty", location, key))
				}
				for _, value := range values {
					problems = append(problems, validateValue(packageName, section, "list", key, value)...)
				}
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func validateValue(packageName string, section UCISection, kind, key string, value UCIValue) []string {
	location := fmt.Sprintf("%s %s %s.%s", packageName, kind, sectionIdentity(section), key)
	var problems []string
	if !identifierRE.MatchString(key) {
		problems = append(problems, location+" has invalid key")
	}
	if (value.Literal == nil) == (value.SecretRef == "") {
		problems = append(problems, location+" must have exactly one literal or secret_ref")
		return problems
	}
	if value.SecretRef != "" {
		if !strings.HasPrefix(value.SecretRef, "op://") || strings.Count(value.SecretRef, "/") < 4 {
			problems = append(problems, location+" secret_ref must use op://vault/item/field syntax")
		}
		return problems
	}
	if strings.ContainsAny(*value.Literal, "\x00\r\n") {
		problems = append(problems, location+" literal must be one line without NUL")
	}
	if isSensitiveOption(packageName, key, *value.Literal) {
		problems = append(problems, location+" must use a 1Password secret_ref, not a literal")
	}
	return problems
}

func isSensitiveOption(packageName, key, value string) bool {
	lower := strings.ToLower(key)
	if packageName == "wireless" && lower == "key" {
		return true
	}
	if packageName == "uhttpd" && (lower == "key" || lower == "cert") && strings.HasPrefix(value, "/etc/") {
		return false
	}
	if packageName == "dropbear" && (lower == "passwordauth" || lower == "rootpasswordauth") {
		return false
	}
	return strings.Contains(lower, "password") || strings.Contains(lower, "passwd") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "private") || lower == "psk"
}

func sectionIdentity(section UCISection) string {
	if section.Name != "" {
		return section.Name
	}
	return "<anonymous:" + section.Type + ">"
}

type secretResolver func(context.Context, string) (string, error)

func defaultServiceAccountTokenFile() string {
	if configured := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN_FILE"); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "homelab-private", "op-service-account-token")
}

func loadServiceAccountToken(path string) (string, error) {
	if path == "" {
		token := os.Getenv("OP_SERVICE_ACCOUNT_TOKEN")
		if token == "" {
			return "", errors.New("no service-account token file or OP_SERVICE_ACCOUNT_TOKEN is configured")
		}
		return token, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect service-account token file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("service-account token file %s must be a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("service-account token file %s permissions must not grant group or other access", path)
	}
	if info.Size() > 64*1024 {
		return "", fmt.Errorf("service-account token file %s is unexpectedly large", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read service-account token file %s: %w", path, err)
	}
	token := strings.TrimSuffix(strings.TrimSuffix(string(content), "\n"), "\r")
	if token == "" {
		return "", fmt.Errorf("service-account token file %s is empty", path)
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("service-account token file %s must contain exactly one token", path)
	}
	if !strings.HasPrefix(token, "ops_") {
		return "", fmt.Errorf("service-account token file %s does not contain an ops_ token", path)
	}
	return token, nil
}

func serviceAccountEnvironment(token string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "OP_SERVICE_ACCOUNT_TOKEN=") ||
			strings.HasPrefix(entry, "OP_CONNECT_HOST=") ||
			strings.HasPrefix(entry, "OP_CONNECT_TOKEN=") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "OP_SERVICE_ACCOUNT_TOKEN="+token)
}

func opResolver(opPath, tokenFile string) secretResolver {
	var once sync.Once
	var token string
	var tokenErr error
	return func(ctx context.Context, reference string) (string, error) {
		once.Do(func() {
			token, tokenErr = loadServiceAccountToken(tokenFile)
		})
		if tokenErr != nil {
			return "", tokenErr
		}
		command := exec.CommandContext(ctx, opPath, "read", "--no-newline", reference)
		command.Env = serviceAccountEnvironment(token)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		value, err := command.Output()
		if err != nil {
			safeStderr := strings.ReplaceAll(strings.TrimSpace(stderr.String()), token, "<redacted>")
			return "", fmt.Errorf("resolve %s: %w: %s", reference, err, safeStderr)
		}
		if bytes.ContainsAny(value, "\x00\r\n") {
			return "", fmt.Errorf("resolve %s: secret must be one line without NUL", reference)
		}
		return string(value), nil
	}
}

func renderPackage(ctx context.Context, pkg UCIPackage, resolve secretResolver, redact bool) ([]byte, error) {
	var output strings.Builder
	for _, section := range pkg.Sections {
		output.WriteString("config ")
		output.WriteString(section.Type)
		if section.Name != "" {
			output.WriteByte(' ')
			output.WriteString(quoteUCI(section.Name))
		}
		output.WriteString("\n")
		optionKeys := sortedKeys(section.Options)
		for _, key := range optionKeys {
			value, err := materializeValue(ctx, section.Options[key], resolve, redact)
			if err != nil {
				return nil, fmt.Errorf("%s option %s.%s: %w", pkg.Package, sectionIdentity(section), key, err)
			}
			fmt.Fprintf(&output, "\toption %s %s\n", key, quoteUCI(value))
		}
		listKeys := sortedKeys(section.Lists)
		for _, key := range listKeys {
			for _, listValue := range section.Lists[key] {
				value, err := materializeValue(ctx, listValue, resolve, redact)
				if err != nil {
					return nil, fmt.Errorf("%s list %s.%s: %w", pkg.Package, sectionIdentity(section), key, err)
				}
				fmt.Fprintf(&output, "\tlist %s %s\n", key, quoteUCI(value))
			}
		}
		output.WriteByte('\n')
	}
	return []byte(output.String()), nil
}

func materializeValue(ctx context.Context, value UCIValue, resolve secretResolver, redact bool) (string, error) {
	if value.SecretRef == "" {
		return *value.Literal, nil
	}
	if redact {
		return value.SecretRef, nil
	}
	if resolve == nil {
		return "", errors.New("secret resolution is required")
	}
	return resolve(ctx, value.SecretRef)
}

func quoteUCI(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return `"` + replacer.Replace(value) + `"`
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func verifyFirmware(profile *LoadedProfile, imagePath, expectedSHA string) error {
	if filepath.Base(imagePath) != profile.Profile.Firmware.Image {
		return fmt.Errorf("image filename %q does not match profile %q", filepath.Base(imagePath), profile.Profile.Firmware.Image)
	}
	if expectedSHA == "" {
		expectedSHA = profile.Profile.Firmware.SHA256
	}
	if expectedSHA != profile.Profile.Firmware.SHA256 {
		return errors.New("requested checksum does not match the reviewed profile checksum")
	}
	file, err := os.Open(imagePath)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expectedSHA {
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", actual, expectedSHA)
	}
	return nil
}
