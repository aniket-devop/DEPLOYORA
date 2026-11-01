package helm

import (
	"fmt"
	"strings"
)

// ReleaseName turns a chart name into a release name: lowercase letters, digits
// and hyphens, no leading or trailing hyphen, and at most 53 characters (Helm's
// limit).
func ReleaseName(chartName string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToLower(chartName) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > maxReleaseLen {
		name = strings.TrimRight(name[:maxReleaseLen], "-")
	}
	if name == "" {
		return "", fmt.Errorf("%w: chart name %q cannot be turned into a release name", ErrInvalid, chartName)
	}
	return name, nil
}
