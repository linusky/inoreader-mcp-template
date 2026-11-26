//go:build darwin

package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type darwinStore struct{}

func newPlatformStore() Store {
	return &darwinStore{}
}

// Set stores a secret in the macOS Keychain using `security -i` (interactive mode)
// so that the secret payload is piped over stdin and never exposed in process argv.
func (d *darwinStore) Set(service, account string, secret []byte) error {
	if err := validateKey(service, account); err != nil {
		return err
	}
	encoded := encodeSecret(secret)
	cmdLine := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", service, account, encoded)

	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(cmdLine)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return fmt.Errorf("macOS keychain save failed (%s): %w", msg, err)
		}
		return fmt.Errorf("macOS keychain save failed: %w", err)
	}
	// `security -i` exits 0 even if an interactive subcommand fails (e.g. locked keychain).
	// When stdin is not a TTY, stderr is empty on success, so treat any stderr output as an error.
	if msg := sanitizeKeychainError(stderr.String()); msg != "" {
		return fmt.Errorf("macOS keychain save failed: %s", msg)
	}
	return nil
}

func (d *darwinStore) Get(service, account string) ([]byte, error) {
	if err := validateKey(service, account); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil, ErrNotFound
		}
		if strings.Contains(strings.ToLower(stderr.String()), "could not be found") {
			return nil, ErrNotFound
		}
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return nil, fmt.Errorf("macOS keychain read failed (%s): %w", msg, err)
		}
		return nil, fmt.Errorf("macOS keychain read failed: %w", err)
	}
	return decodeSecret(stdout.String())
}

func (d *darwinStore) Delete(service, account string) error {
	if err := validateKey(service, account); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/security", "delete-generic-password", "-s", service, "-a", account)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil
		}
		if strings.Contains(strings.ToLower(stderr.String()), "could not be found") {
			return nil
		}
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return fmt.Errorf("macOS keychain delete failed (%s): %w", msg, err)
		}
		return fmt.Errorf("macOS keychain delete failed: %w", err)
	}
	return nil
}
