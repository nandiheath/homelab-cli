package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionCommandFormsReportInjectedVersion(t *testing.T) {
	originalVersion := version
	version = "0.2.0"
	t.Cleanup(func() { version = originalVersion })

	for _, args := range [][]string{{"version"}, {"--version"}} {
		got := captureStdout(t, func() {
			if err := run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
		})
		if got != "0.2.0\n" {
			t.Fatalf("run(%q) output = %q, want %q", args, got, "0.2.0\\n")
		}
	}
}

func captureStdout(t *testing.T, action func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	action()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestRenderPackageEscapesResolvedSecret(t *testing.T) {
	secret := UCIValue{SecretRef: "op://ExampleVault/ExampleRouter/password"}
	pkg := UCIPackage{Package: "wireless", Sections: []UCISection{{
		Type:    "wifi-iface",
		Name:    "ap",
		Options: map[string]UCIValue{"key": secret},
	}}}
	resolver := func(context.Context, string) (string, error) {
		return `p$a"ss`, nil
	}
	rendered, err := renderPackage(context.Background(), pkg, resolver, false)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "op://") {
		t.Fatalf("resolved render retained reference: %s", text)
	}
	if !strings.Contains(text, `option key "p\$a\"ss"`) {
		t.Fatalf("resolved secret was not safely UCI-quoted: %s", text)
	}
}

func TestPlanDoesNotDiscloseSecret(t *testing.T) {
	desiredSecret := "new-secret"
	actualSecret := "old-secret"
	desired := UCIPackage{Package: "wireless", Sections: []UCISection{{
		Type: "wifi-iface", Name: "ap", Options: map[string]UCIValue{
			"key": {Literal: &desiredSecret, SecretRef: "op://ExampleVault/ExampleRouter/password"},
		},
	}}}
	actual := UCIPackage{Package: "wireless", Sections: []UCISection{{
		Type: "wifi-iface", Name: "ap", Options: map[string]UCIValue{
			"key": {Literal: &actualSecret},
		},
	}}}
	result := comparePackage(desired, actual, true)
	joined := strings.Join(result.Lines, "\n")
	if strings.Contains(joined, desiredSecret) || strings.Contains(joined, actualSecret) {
		t.Fatalf("plan disclosed a secret: %s", joined)
	}
	if result.Drift != 1 || !strings.Contains(joined, "managed secret differs") {
		t.Fatalf("unexpected secret diff: %#v", result)
	}
}

func TestParseUCIExport(t *testing.T) {
	input := "package network\n\nconfig interface 'lan'\n\toption proto 'static'\n\tlist ipaddr '192.0.2.1/24'\n"
	pkg, err := parseUCIExport("network", input)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Sections) != 1 || pkg.Sections[0].Name != "lan" || valueString(pkg.Sections[0].Options["proto"]) != "static" {
		t.Fatalf("unexpected package: %#v", pkg)
	}
	if got := valueString(pkg.Sections[0].Lists["ipaddr"][0]); got != "192.0.2.1/24" {
		t.Fatalf("unexpected list value: %q", got)
	}
}

func TestProfileRejectsPlaintextWirelessKey(t *testing.T) {
	directory, _ := writeTestProfile(t)
	wirelessPath := filepath.Join(directory, "uci", "wireless.json")
	plaintext := UCIPackage{Package: "wireless", Sections: []UCISection{{
		Type: "wifi-iface", Name: "ap", Options: map[string]UCIValue{
			"key": literalValue("plaintext-password"),
		},
	}}}
	writeJSON(t, wirelessPath, plaintext)
	_, err := loadProfile(directory)
	if err == nil || !strings.Contains(err.Error(), "must use a 1Password secret_ref") {
		t.Fatalf("expected plaintext secret rejection, got %v", err)
	}
}

func TestApplyDryRunDoesNotRequireTargetOrAuthorization(t *testing.T) {
	directory, _ := writeTestProfile(t)
	if err := applyCommand(context.Background(), []string{"--profile", directory, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRejectsMissingAuthorizationBeforeConnection(t *testing.T) {
	directory, _ := writeTestProfile(t)
	err := applyCommand(context.Background(), []string{"--profile", directory, "--target", "root@192.0.2.1"})
	if err == nil || !strings.Contains(err.Error(), "authorization must equal APPLY:test-router") {
		t.Fatalf("expected authorization failure, got %v", err)
	}
}

func TestFirmwareVerificationUsesReviewedNameAndDigest(t *testing.T) {
	directory, imagePath := writeTestProfile(t)
	profile, err := loadProfile(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyFirmware(profile, imagePath, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFirmware(profile, imagePath, ""); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("expected digest failure, got %v", err)
	}
}

func TestServiceAccountTokenFilePopulatesOpChild(t *testing.T) {
	directory := t.TempDir()
	token := "ops_" + strings.Repeat("x", 848)
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opPath := filepath.Join(directory, "op")
	script := "#!/bin/sh\n" +
		"if [ \"$OP_SERVICE_ACCOUNT_TOKEN\" != '" + token + "' ]; then exit 8; fi\n" +
		"if [ -n \"$OP_CONNECT_HOST$OP_CONNECT_TOKEN\" ]; then exit 9; fi\n" +
		"printf 'resolved-value'\n"
	if err := os.WriteFile(opPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OP_CONNECT_HOST", "https://must-not-be-used.invalid")
	t.Setenv("OP_CONNECT_TOKEN", "must-not-be-used")
	resolver := opResolver(opPath, tokenPath)
	value, err := resolver(context.Background(), "op://ExampleVault/example_wifi/password")
	if err != nil {
		t.Fatal(err)
	}
	if value != "resolved-value" {
		t.Fatalf("unexpected resolved value %q", value)
	}
}

func TestServiceAccountTokenFileRejectsBroadPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("ops_test-token"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := loadServiceAccountToken(path)
	if err == nil || !strings.Contains(err.Error(), "must not grant group or other access") {
		t.Fatalf("expected permission rejection, got %v", err)
	}
}

func TestServiceAccountTokenIsRedactedFromOpErrors(t *testing.T) {
	directory := t.TempDir()
	token := "ops_sensitive-test-token"
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	opPath := filepath.Join(directory, "op")
	if err := os.WriteFile(opPath, []byte("#!/bin/sh\nprintf '%s' \"$OP_SERVICE_ACCOUNT_TOKEN\" >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := opResolver(opPath, tokenPath)(context.Background(), "op://ExampleVault/example_item/field")
	if err == nil {
		t.Fatal("expected op failure")
	}
	if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("token was not redacted: %v", err)
	}
}

func TestUploadUsesLegacySCPForDropbear(t *testing.T) {
	directory := t.TempDir()
	capturePath := filepath.Join(directory, "arguments")
	scpPath := filepath.Join(directory, "scp")
	if err := os.WriteFile(scpPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_PATH\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("CAPTURE_PATH", capturePath)
	client := &sshClient{
		target:         targetSpec{User: "root", Host: "192.0.2.1", Port: 22},
		knownHostsPath: filepath.Join(directory, "known_hosts"),
	}
	if err := client.upload(context.Background(), "local.bin", "/tmp/remote.bin"); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	if first := strings.Split(strings.TrimSpace(string(arguments)), "\n")[0]; first != "-O" {
		t.Fatalf("legacy SCP mode was not selected first, got %q", first)
	}
}

func writeTestProfile(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "uci"), 0700); err != nil {
		t.Fatal(err)
	}
	imageName := "openwrt-test-sysupgrade.bin"
	imagePath := filepath.Join(directory, imageName)
	image := []byte("test firmware")
	if err := os.WriteFile(imagePath, image, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(image)
	profile := Profile{
		SchemaVersion:   1,
		Name:            "test-router",
		Description:     "test",
		BoardNames:      []string{"vendor,test-router"},
		ManagedPackages: []string{"wireless"},
		PostApplyHost:   "192.0.2.1",
		SSH: SSHSpec{
			User: "root", Port: 22, PublicKey: "authorized.pub",
			KnownHostFingerprint: "SHA256:test",
		},
		Firmware: Firmware{
			Version: "1.0", Target: "test/target", Image: imageName,
			SHA256: hex.EncodeToString(digest[:]),
		},
	}
	writeJSON(t, filepath.Join(directory, "profile.json"), profile)
	wireless := UCIPackage{Package: "wireless", Sections: []UCISection{{
		Type: "wifi-iface", Name: "ap", Options: map[string]UCIValue{
			"key": {SecretRef: "op://ExampleVault/ExampleRouter/password"},
		},
	}}}
	writeJSON(t, filepath.Join(directory, "uci", "wireless.json"), wireless)
	if err := os.WriteFile(filepath.Join(directory, "authorized.pub"), []byte("ssh-rsa AAAA test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return directory, imagePath
}

func literalValue(value string) UCIValue {
	return UCIValue{Literal: &value}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}
