//go:build unix

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockCredentials takes the credentials file's lock, held by one cadre at
// a time, and returns what releases it: a refresh reads, refreshes and
// writes the login under it, so two never spend the same refresh token.
func lockCredentials() (unlock func(), err error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
