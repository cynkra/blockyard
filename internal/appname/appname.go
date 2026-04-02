// Package appname provides app name validation shared by the API and UI packages.
package appname

import "fmt"

// Validate checks that name is a valid URL-safe slug: 1-63 lowercase letters,
// digits, and hyphens, starting with a letter and not ending with a hyphen.
func Validate(name string) error {
	if len(name) == 0 || len(name) > 63 {
		return fmt.Errorf("name must be 1-63 characters")
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return fmt.Errorf("name must contain only lowercase letters, digits, and hyphens")
		}
	}
	if name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("name must start with a lowercase letter")
	}
	if name[len(name)-1] == '-' {
		return fmt.Errorf("name must not end with a hyphen")
	}
	return nil
}
