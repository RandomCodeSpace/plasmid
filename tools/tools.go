//go:build tools

package tools

import (
	// Keep the locked gitleaks version available for the CI scanner build.
	_ "github.com/zricethezav/gitleaks/v8"
	// Keep the locked govulncheck version available for the CI scanner build.
	_ "golang.org/x/vuln/cmd/govulncheck"
)
