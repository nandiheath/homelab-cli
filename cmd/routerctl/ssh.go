package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var hostRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type targetSpec struct {
	User string
	Host string
	Port int
}

func parseTarget(raw, defaultUser string, defaultPort int) (targetSpec, error) {
	user := defaultUser
	host := raw
	if before, after, found := strings.Cut(raw, "@"); found {
		user = before
		host = after
	}
	if user == "" || !identifierRE.MatchString(user) {
		return targetSpec{}, errors.New("target has an invalid SSH user")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if net.ParseIP(host) == nil && !hostRE.MatchString(host) {
		return targetSpec{}, errors.New("target host must be an IP address or DNS name")
	}
	if defaultPort < 1 || defaultPort > 65535 {
		return targetSpec{}, errors.New("target port is invalid")
	}
	return targetSpec{User: user, Host: host, Port: defaultPort}, nil
}

func (t targetSpec) destination() string {
	host := t.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return t.User + "@" + host
}

type sshClient struct {
	target         targetSpec
	knownHostsPath string
	tempDir        string
}

func inspectFingerprints(ctx context.Context, target targetSpec) ([]string, []byte, error) {
	args := []string{"-T", "5", "-p", strconv.Itoa(target.Port), target.Host}
	command := exec.CommandContext(ctx, "ssh-keyscan", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	keyscan, err := command.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("ssh-keyscan: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(bytes.TrimSpace(keyscan)) == 0 {
		return nil, nil, errors.New("ssh-keyscan returned no host keys")
	}
	tempDir, err := os.MkdirTemp("", "routerctl-keyscan-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tempDir)
	path := filepath.Join(tempDir, "known_hosts")
	if err := os.WriteFile(path, keyscan, 0600); err != nil {
		return nil, nil, err
	}
	fingerprintCommand := exec.CommandContext(ctx, "ssh-keygen", "-E", "sha256", "-lf", path)
	fingerprintOutput, err := fingerprintCommand.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("ssh-keygen: %w", err)
	}
	var fingerprints []string
	for _, line := range strings.Split(strings.TrimSpace(string(fingerprintOutput)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[1], "SHA256:") {
			fingerprints = append(fingerprints, fields[1])
		}
	}
	if len(fingerprints) == 0 {
		return nil, nil, errors.New("no SHA256 host-key fingerprint found")
	}
	return fingerprints, keyscan, nil
}

func newSSHClient(ctx context.Context, target targetSpec, expectedFingerprint string) (*sshClient, error) {
	if !strings.HasPrefix(expectedFingerprint, "SHA256:") {
		return nil, errors.New("a pinned SHA256 host-key fingerprint is required")
	}
	fingerprints, keyscan, err := inspectFingerprints(ctx, target)
	if err != nil {
		return nil, err
	}
	matched := false
	for _, fingerprint := range fingerprints {
		if fingerprint == expectedFingerprint {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("host-key mismatch for %s: observed %s, expected %s", target.Host, strings.Join(fingerprints, ", "), expectedFingerprint)
	}
	tempDir, err := os.MkdirTemp("", "routerctl-ssh-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(tempDir, "known_hosts")
	if err := os.WriteFile(path, keyscan, 0600); err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	return &sshClient{target: target, knownHostsPath: path, tempDir: tempDir}, nil
}

func (c *sshClient) close() {
	if c != nil {
		_ = os.RemoveAll(c.tempDir)
	}
}

func (c *sshClient) sshArgs(batch bool) []string {
	batchValue := "yes"
	if !batch {
		batchValue = "no"
	}
	return []string{
		"-F", "/dev/null",
		"-o", "BatchMode=" + batchValue,
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + c.knownHostsPath,
		"-p", strconv.Itoa(c.target.Port),
	}
}

func (c *sshClient) output(ctx context.Context, remoteCommand string) ([]byte, error) {
	args := append(c.sshArgs(true), c.target.destination(), remoteCommand)
	command := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w: %s", c.target.Host, err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func (c *sshClient) run(ctx context.Context, remoteCommand string, tolerateDisconnect bool) error {
	args := append(c.sshArgs(true), c.target.destination(), remoteCommand)
	command := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil && !tolerateDisconnect {
		return fmt.Errorf("ssh %s: %w: %s", c.target.Host, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *sshClient) runInteractive(ctx context.Context, remoteCommand string) error {
	args := append(c.sshArgs(false), "-tt", c.target.destination(), remoteCommand)
	command := exec.CommandContext(ctx, "ssh", args...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func (c *sshClient) upload(ctx context.Context, localPath, remotePath string) error {
	args := []string{
		"-O",
		"-F", "/dev/null",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + c.knownHostsPath,
		"-P", strconv.Itoa(c.target.Port),
		localPath,
		c.target.destination() + ":" + remotePath,
	}
	command := exec.CommandContext(ctx, "scp", args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("scp to %s: %w: %s", c.target.Host, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (c *sshClient) board(ctx context.Context) (boardInfo, error) {
	output, err := c.output(ctx, "ubus call system board")
	if err != nil {
		return boardInfo{}, err
	}
	var board boardInfo
	if err := json.Unmarshal(output, &board); err != nil {
		return boardInfo{}, fmt.Errorf("decode system board: %w", err)
	}
	return board, nil
}

type boardInfo struct {
	BoardName string `json:"board_name"`
	Model     string `json:"model"`
	Release   struct {
		Version string `json:"version"`
		Target  string `json:"target"`
	} `json:"release"`
}

func requireSupportedBoard(profile *LoadedProfile, board boardInfo) error {
	for _, allowed := range profile.Profile.BoardNames {
		if board.BoardName == allowed {
			return nil
		}
	}
	return fmt.Errorf("router board %q is not allowed by profile %q", board.BoardName, profile.Profile.Name)
}

func waitForSSH(ctx context.Context, profile *LoadedProfile, target targetSpec, fingerprint string, timeout time.Duration) (*sshClient, error) {
	deadline := time.Now().Add(timeout)
	var lastError error
	for time.Now().Before(deadline) {
		client, err := newSSHClient(ctx, target, fingerprint)
		if err == nil {
			if _, err = client.output(ctx, "true"); err == nil {
				return client, nil
			}
			client.close()
		}
		lastError = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("router did not become reachable before timeout: %w", lastError)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
