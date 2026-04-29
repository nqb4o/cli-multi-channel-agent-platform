// Generate a Claude Code-compatible plugin directory from resolved skills.
//
// Claude Code consumes skills via its plugin system (--plugin-dir). Layout:
//
//	/tmp/claude_plugins_<run_id>/
//	└── <plugin-name>/
//	    ├── .claude-plugin/
//	    │   └── plugin.json
//	    └── skills/
//	        └── <slug>/
//	            ├── SKILL.md
//	            └── (any sibling files copied from the source skill dir)
//
// Adapted from openclaw/src/cli/claude-config.ts and the real plugin layout at
// ~/.claude/plugins/marketplaces/claude-plugins-official/plugins/playground/.
package skills

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPluginName is the plugin folder name used when none is specified.
const DefaultPluginName = "openclaw-runtime"

// PluginBaseDir is the default parent directory for plugin dirs.
const PluginBaseDir = "/tmp"

var ignoredCopyDirs = map[string]bool{
	".git":        true,
	"__pycache__": true,
	"node_modules": true,
	".venv":       true,
}

// PluginDirEntry is one skill copied into the plugin dir.
type PluginDirEntry struct {
	Slug       string
	SourcePath string
	DestPath   string
}

// PluginDir is the result of GeneratePluginDir.
type PluginDir struct {
	Root       string // pass this to Claude as --plugin-dir
	PluginRoot string // Root/<plugin_name>
	PluginName string
	Entries    []PluginDirEntry
}

// GeneratePluginDir materialises the Claude plugin layout under
// baseDir/claude_plugins_<run_id>/.
//
// base_dir defaults to /tmp. Tests pass a tmp dir to avoid polluting /tmp.
func GeneratePluginDir(
	skills []*ResolvedSkill,
	runID string,
	baseDir string,
	pluginName string,
	description string,
) (*PluginDir, error) {
	if runID == "" || strings.Contains(runID, "/") || strings.Contains(runID, "..") {
		return nil, fmt.Errorf("invalid run_id %q", runID)
	}
	if baseDir == "" {
		baseDir = PluginBaseDir
	}
	if pluginName == "" {
		pluginName = DefaultPluginName
	}

	root := filepath.Join(baseDir, "claude_plugins_"+runID)
	pluginRoot := filepath.Join(root, pluginName)

	// Wipe + recreate so reruns of the same run_id are reproducible.
	if err := os.RemoveAll(root); err != nil {
		return nil, fmt.Errorf("plugin_dir cleanup: %w", err)
	}
	skillsRoot := filepath.Join(pluginRoot, "skills")
	if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
		return nil, fmt.Errorf("plugin_dir mkdir: %w", err)
	}

	var entries []PluginDirEntry
	for _, resolved := range skills {
		manifest := resolved.Manifest
		slug := manifest.Name
		destSkillDir := filepath.Join(skillsRoot, slug)
		if err := os.MkdirAll(destSkillDir, 0o755); err != nil {
			return nil, fmt.Errorf("plugin_dir skill mkdir %q: %w", slug, err)
		}

		srcSkillDir := resolved.SkillDir
		info, err := os.Stat(srcSkillDir)
		if err == nil && info.IsDir() {
			if err := copySkillTree(srcSkillDir, destSkillDir); err != nil {
				return nil, fmt.Errorf("plugin_dir copy %q: %w", slug, err)
			}
		} else {
			// Fall back to serialising the manifest so an in-memory skill
			// still produces a valid layout.
			text := serializeManifest(manifest)
			dest := filepath.Join(destSkillDir, "SKILL.md")
			if err := os.WriteFile(dest, []byte(text), 0o644); err != nil {
				return nil, fmt.Errorf("plugin_dir write manifest %q: %w", slug, err)
			}
		}

		entries = append(entries, PluginDirEntry{
			Slug:       slug,
			SourcePath: manifest.SourcePath,
			DestPath:   filepath.Join(destSkillDir, "SKILL.md"),
		})
	}

	// plugin.json: minimum Claude Code expects.
	pluginMetaDir := filepath.Join(pluginRoot, ".claude-plugin")
	if err := os.MkdirAll(pluginMetaDir, 0o755); err != nil {
		return nil, fmt.Errorf("plugin_dir meta mkdir: %w", err)
	}
	if description == "" {
		description = fmt.Sprintf("Runtime-resolved skill bundle (%d skill(s))", len(entries))
	}
	pluginMeta := map[string]any{
		"name":        pluginName,
		"description": description,
	}
	metaJSON, err := json.MarshalIndent(pluginMeta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("plugin.json marshal: %w", err)
	}
	metaJSON = append(metaJSON, '\n')
	if err := os.WriteFile(filepath.Join(pluginMetaDir, "plugin.json"), metaJSON, 0o644); err != nil {
		return nil, fmt.Errorf("plugin.json write: %w", err)
	}

	return &PluginDir{
		Root:       root,
		PluginRoot: pluginRoot,
		PluginName: pluginName,
		Entries:    entries,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// copySkillTree copies src → dest recursively, skipping ignoredCopyDirs.
func copySkillTree(src, dest string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		destPath := filepath.Join(dest, entry.Name())
		if entry.IsDir() {
			if ignoredCopyDirs[entry.Name()] {
				continue
			}
			if err := os.MkdirAll(destPath, 0o755); err != nil {
				return err
			}
			if err := copySkillTree(srcPath, destPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, destPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// serializeManifest round-trips a SkillManifest back to a minimal SKILL.md.
// Used as a fallback when the source dir is missing.
func serializeManifest(m *SkillManifest) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("name: " + m.Name + "\n")
	sb.WriteString("version: " + m.Version + "\n")
	sb.WriteString("description: " + m.Description + "\n")
	sb.WriteString("when_to_use: " + m.WhenToUse + "\n")
	if len(m.AllowedTools) > 0 {
		sb.WriteString("allowed_tools: [" + strings.Join(m.AllowedTools, ", ") + "]\n")
	}
	if len(m.RequiredEnv) > 0 {
		sb.WriteString("required_env: [" + strings.Join(m.RequiredEnv, ", ") + "]\n")
	}
	sb.WriteString("---\n")
	if m.Body != "" {
		sb.WriteString("\n")
		sb.WriteString(m.Body)
		sb.WriteString("\n")
	}
	return sb.String()
}
