// This file resolves CLI clients against the server managed by "chorus up".
package main

import (
	"flag"
	"fmt"
	"os"
)

// clientBaseURL uses an explicit -addr, otherwise the address saved by "up".
// All callers register and parse -addr before calling it. A missing saved file
// uses the server default; an unreadable or invalid file is an error.
func clientBaseURL(fs *flag.FlagSet, addr string) (string, error) {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			explicit = true
		}
	})
	if explicit {
		return localBaseURL(addr)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	args, err := loadServeArgs(home)
	if err != nil {
		return "", err
	}
	cfg, err := parseServerFlags(args)
	if err != nil {
		return "", err
	}
	return localBaseURL(cfg.addr)
}
