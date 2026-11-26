//go:build !darwin && !linux && !windows

package keychain

import "errors"

type unsupportedStore struct{}

func newPlatformStore() Store {
	return &unsupportedStore{}
}

func (u *unsupportedStore) Get(string, string) ([]byte, error) {
	return nil, errors.New("os keychain is not supported on this platform")
}

func (u *unsupportedStore) Set(string, string, []byte) error {
	return errors.New("os keychain is not supported on this platform")
}

func (u *unsupportedStore) Delete(string, string) error {
	return errors.New("os keychain is not supported on this platform")
}
