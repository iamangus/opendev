// Package identity validates user-controlled repository and Git identifiers.
package identity

import (
	"fmt"
	"regexp"
	"strings"
)

var repository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
var branch = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

func Repository(value string) error {
	if !repository.MatchString(value) || strings.Contains(value, "..") || strings.HasSuffix(value, ".git") {
		return fmt.Errorf("invalid repository name %q", value)
	}
	return nil
}

func Branch(value string) error {
	if len(value) > 240 || !branch.MatchString(value) || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") {
		return fmt.Errorf("invalid branch name %q", value)
	}
	for _, part := range strings.Split(value, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("invalid branch name %q", value)
		}
	}
	return nil
}
