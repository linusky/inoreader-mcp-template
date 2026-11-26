//go:build linux

package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type linuxStore struct{}

func newPlatformStore() Store {
	return &linuxStore{}
}

func secretToolBin() string {
	if _, err := os.Stat("/usr/bin/secret-tool"); err == nil {
		return "/usr/bin/secret-tool"
	}
	return "secret-tool"
}

// Set stores a secret via libsecret (`secret-tool`), reading the secret from stdin
// so it never appears in process argv.
func (l *linuxStore) Set(service, account string, secret []byte) error {
	if err := validateKey(service, account); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	encoded := encodeSecret(secret)
	cmd := exec.CommandContext(ctx, secretToolBin(), "store", "--label="+service+" ("+account+")", "service", service, "account", account)
	cmd.Stdin = strings.NewReader(encoded)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return fmt.Errorf("linux keychain save failed (%s): %w", msg, err)
		}
		return fmt.Errorf("linux keychain save failed (ensure secret-tool / libsecret is installed): %w", err)
	}
	return nil
}

func (l *linuxStore) Get(service, account string) ([]byte, error) {
	if err := validateKey(service, account); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, secretToolBin(), "lookup", "service", service, "account", account)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.TrimSpace(stdout.String()) == "" && strings.TrimSpace(stderr.String()) == "" {
			return nil, ErrNotFound
		}
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return nil, fmt.Errorf("linux keychain read failed (%s): %w", msg, err)
		}
		return nil, fmt.Errorf("linux keychain read failed: %w", err)
	}
	return decodeSecret(stdout.String())
}

func (l *linuxStore) Delete(service, account string) error {
	if err := validateKey(service, account); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, secretToolBin(), "clear", "service", service, "account", account)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return fmt.Errorf("linux keychain delete failed (%s): %w", msg, err)
		}
		return fmt.Errorf("linux keychain delete failed: %w", err)
	}
	return nil
}
