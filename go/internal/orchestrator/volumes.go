// Volume mount specs.
//
// The four mount paths below are part of the **public contract** with
// F02/F03/F04 (the CLI backends). They expect to find provider auth state
// at exactly these paths and will fail closed if we move them. See
// docs/01-overview.md "Storage rules in sandbox" and
// docs/features/F01-sandbox-orchestrator.md "Out-of-band notes".
package orchestrator

import (
	"fmt"
	"strings"
)

// Public contract — DO NOT change without coordinating with F02/F03/F04.
const (
	CodexAuthPath  = "/home/user/.codex"
	GeminiAuthPath = "/home/user/.gemini"
	ClaudeAuthPath = "/home/user/.claude"
	WorkspacePath  = "/home/user/workspace"
)

// VolumeSpec is a single volume mount.
//
// LogicalName is the per-provider logical name (e.g. "codex-auth"). The
// orchestrator combines it with the user_id to derive the actual Daytona
// volume name: u-<user_id>-<logical_name>.
type VolumeSpec struct {
	LogicalName string
	MountPath   string
}

// UserVolumeSpecs lists the per-user volumes. Each user gets their own
// copy at create() time.
var UserVolumeSpecs = []VolumeSpec{
	{LogicalName: "codex-auth", MountPath: CodexAuthPath},
	{LogicalName: "gemini-auth", MountPath: GeminiAuthPath},
	{LogicalName: "claude-auth", MountPath: ClaudeAuthPath},
	{LogicalName: "workspace", MountPath: WorkspacePath},
}

// VolumeNameFor returns the deterministic Daytona volume name for a user +
// logical mount. Stable across sandbox destroy/recreate cycles so volumes
// survive. Validates user_id to keep the volume namespace tidy.
func VolumeNameFor(userID, logicalName string) (string, error) {
	if userID == "" || strings.ContainsAny(userID, " /:") {
		return "", fmt.Errorf("invalid user_id: %q", userID)
	}
	return "u-" + userID + "-" + logicalName, nil
}
