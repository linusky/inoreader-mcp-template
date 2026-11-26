//go:build windows

package keychain

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type windowsStore struct{}

func newPlatformStore() Store {
	return &windowsStore{}
}

func powershellBin() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		candidate := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "powershell.exe"
}

func credentialPath(service, account string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, service, account+".dpapi"), nil
}

// Set encrypts the secret with Windows DPAPI (CurrentUser scope) via PowerShell
// reading the secret from stdin so it never appears in process argv.
func (w *windowsStore) Set(service, account string, secret []byte) error {
	if err := validateKey(service, account); err != nil {
		return err
	}
	target, err := credentialPath(service, account)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write credential to symlink")
	}

	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	tmpPath := target + ".tmp"
	encoded := encodeSecret(secret)
	script := `
$ErrorActionPreference = 'Stop'
$plain = [Console]::In.ReadToEnd().Trim()
$sec = ConvertTo-SecureString -String $plain -AsPlainText -Force
$enc = ConvertFrom-SecureString -SecureString $sec
[System.IO.File]::WriteAllText($env:INOREADER_CRED_TMP, $enc)
Move-Item -LiteralPath $env:INOREADER_CRED_TMP -Destination $env:INOREADER_CRED_PATH -Force
`
	cmd := exec.CommandContext(ctx, powershellBin(), "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Stdin = strings.NewReader(encoded)
	cmd.Env = append(os.Environ(),
		"INOREADER_CRED_PATH="+target,
		"INOREADER_CRED_TMP="+tmpPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmpPath)
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return fmt.Errorf("windows DPAPI save failed (%s): %w", msg, err)
		}
		return fmt.Errorf("windows DPAPI save failed: %w", err)
	}
	return nil
}

func (w *windowsStore) Get(service, account string) ([]byte, error) {
	if err := validateKey(service, account); err != nil {
		return nil, err
	}
	target, err := credentialPath(service, account)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return nil, ErrNotFound
	}

	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()

	script := `
$ErrorActionPreference = 'Stop'
$enc = [System.IO.File]::ReadAllText($env:INOREADER_CRED_PATH).Trim()
$sec = ConvertTo-SecureString -String $enc
$bstr = [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec)
try { [Console]::Out.Write([System.Runtime.InteropServices.Marshal]::PtrToStringAuto($bstr)) }
finally { [System.Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
`
	cmd := exec.CommandContext(ctx, powershellBin(), "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "INOREADER_CRED_PATH="+target)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := sanitizeKeychainError(stderr.String()); msg != "" {
			return nil, fmt.Errorf("windows DPAPI read failed (%s): %w", msg, err)
		}
		return nil, fmt.Errorf("windows DPAPI read failed: %w", err)
	}
	return decodeSecret(stdout.String())
}

func (w *windowsStore) Delete(service, account string) error {
	if err := validateKey(service, account); err != nil {
		return err
	}
	target, err := credentialPath(service, account)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("windows DPAPI delete failed: %w", err)
	}
	return nil
}
