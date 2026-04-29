package runtime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// DefaultWorkspaceDir is the production location of the workspace volume
// mount inside a Daytona sandbox.
const DefaultWorkspaceDir = "/home/user/workspace"

// BootstrapFilenames is the canonical order of bootstrap files. The order
// is also the section order in the system prompt — order is part of the
// contract and must match the Python BOOTSTRAP_FILENAMES tuple.
var BootstrapFilenames = []string{
	"AGENTS.md",
	"SOUL.md",
	"IDENTITY.md",
	"USER.md",
	"BOOTSTRAP.md",
}

// PerFileMaxChars is the per-file truncation budget. Long workspace files
// are trimmed so the system prompt stays lean.
//
// The Python tree caps at 24,000 chars; the F05 brief in REFACTOR-GO.md
// references "16KB" for large files. We keep the Python value (24,000) so
// the wire-shape is identical — the brief's 16KB is a rule-of-thumb cap,
// the actual budget the parser enforces is in PER_FILE_MAX_CHARS.
const PerFileMaxChars = 24_000

// truncationMarker is appended when content is trimmed.
const truncationMarker = "\n\n[... truncated by runtime bootstrap (file too long) ...]"

// BootstrapFile is one workspace bootstrap file.
//
// Content == nil when the file is missing or unreadable. Callers use
// Missing rather than checking Content to keep intent explicit (a
// present-but-empty file has Content="" and Missing=false).
type BootstrapFile struct {
	Name    string
	Path    string
	Content *string
	Missing bool
}

// LoadBootstrapFiles reads every file in BootstrapFilenames from
// workspaceDir. Pass workspaceDir = "" to use DefaultWorkspaceDir.
//
// Returns one BootstrapFile per filename, in canonical order. Missing
// files are still included (with Missing=true) — the prompt builder
// skips them, but downstream telemetry / tests can still see which
// workspaces are pre-seeded vs. blank.
func LoadBootstrapFiles(workspaceDir string) []BootstrapFile {
	base := workspaceDir
	if base == "" {
		base = DefaultWorkspaceDir
	}
	out := make([]BootstrapFile, 0, len(BootstrapFilenames))
	for _, name := range BootstrapFilenames {
		filePath := filepath.Join(base, name)
		raw, err := os.ReadFile(filePath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				out = append(out, BootstrapFile{Name: name, Path: filePath, Missing: true})
				continue
			}
			// Other read error — treat as missing.
			out = append(out, BootstrapFile{Name: name, Path: filePath, Missing: true})
			continue
		}
		// Treat invalid UTF-8 as missing rather than crashing.
		if !utf8.Valid(raw) {
			out = append(out, BootstrapFile{Name: name, Path: filePath, Missing: true})
			continue
		}
		trimmed := trimToBudget(string(raw))
		out = append(out, BootstrapFile{
			Name:    name,
			Path:    filePath,
			Content: &trimmed,
			Missing: false,
		})
	}
	return out
}

func trimToBudget(text string) string {
	if len(text) <= PerFileMaxChars {
		return text
	}
	return text[:PerFileMaxChars] + truncationMarker
}

// BuildSystemPrompt assembles the system prompt for a turn.
//
// Layout:
//
//	# Identity
//	You are <name>.
//	<extraPreamble>          // optional; F11 / future hooks inject here
//
//	# AGENTS.md
//	...
//	# SOUL.md
//	...
//	# IDENTITY.md
//	...
//
// Empty / missing files are skipped silently. If every file is empty the
// result is just the identity preamble — that is the "blank workspace"
// case and is intentional (the agent still has a name).
func BuildSystemPrompt(identity IdentityConfig, files []BootstrapFile, extraPreamble string) string {
	var parts []string
	parts = append(parts, "# Identity")
	parts = append(parts, "You are "+identity.Name+".")
	if strings.TrimSpace(extraPreamble) != "" {
		parts = append(parts, strings.TrimSpace(extraPreamble))
	}
	for _, f := range files {
		if f.Missing || f.Content == nil {
			continue
		}
		body := strings.TrimSpace(*f.Content)
		if body == "" {
			continue
		}
		parts = append(parts, "\n# "+f.Name+"\n"+body)
	}
	return strings.TrimSpace(strings.Join(parts, "\n")) + "\n"
}
