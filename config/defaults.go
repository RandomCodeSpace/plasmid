package config

import (
	"path/filepath"
	"time"
)

func defaults(workingDir string) Config {
	return Config{
		Version:    CurrentVersion,
		WorkingDir: workingDir,
		SessionDir: filepath.Join(workingDir, ".plasmid", "sessions"),
		AppName:    "plasmid",
		UserID:     "default",
		LSP: LSP{
			Mode:                  LSPAuto,
			SettleTimeout:         1500 * time.Millisecond,
			InitializeTimeout:     10 * time.Second,
			RequestTimeout:        5 * time.Second,
			FailureThreshold:      3,
			MaxDiagnosticsPerFile: 20,
			Servers: []LSPServer{{
				ID:          "gopls",
				Command:     "gopls",
				Extensions:  []string{".go"},
				RootMarkers: []string{"go.work", "go.mod"},
			}},
		},
		Foreign: Foreign{Enabled: true, Claude: true, Codex: true, Copilot: true},
		Syntax: Syntax{
			PromptCommands:      PromptCommandsTrusted,
			CommandTimeout:      30 * time.Second,
			DocumentTimeout:     60 * time.Second,
			CommandOutputBytes:  8 * 1024,
			DocumentOutputBytes: 32 * 1024,
		},
		Context: Context{
			MaxFileBytes:       16 * 1024,
			MaxBytes:           256 * 1024,
			MaxImportDepth:     4,
			TouchesPerToolCall: 256,
		},
		Tools: Tools{
			CallOutputBytes:    30_000,
			SessionOutputBytes: 400_000,
			BashTimeout:        120 * time.Second,
			BashMaxTimeout:     600 * time.Second,
		},
		Compaction: Compaction{
			TriggerFraction:      0.85,
			TargetFraction:       0.60,
			KeepRecentContents:   12,
			MinimumElisionTokens: 200,
			Calibration:          true,
		},
	}
}
