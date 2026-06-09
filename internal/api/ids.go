package api

import (
	"fmt"
	"regexp"
	"strings"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func toResourceID(prefix, slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return generateID(prefix), nil
	}
	if len(slug) > 64 || !slugPattern.MatchString(slug) {
		return "", fmt.Errorf("id must be lowercase alphanumeric with optional hyphens and max 64 chars")
	}
	return prefix + "_" + strings.ReplaceAll(slug, "-", "_"), nil
}
