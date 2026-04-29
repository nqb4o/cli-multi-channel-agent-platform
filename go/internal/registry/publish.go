package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/openclaw/agent-platform/internal/registry/storage"
	"github.com/openclaw/agent-platform/internal/skills"
)

// MaxTarballBytes caps the upload size at 32 MiB — skills are small text
// bundles; this is already generous for a SKILL.md + helper scripts.
const MaxTarballBytes = 32 * 1024 * 1024

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// PublishError carries a stable error code and a human-readable message.
// The API layer uses the code to pick the HTTP status.
type PublishError struct {
	Code    string // invalid_tarball | invalid_manifest | slug_mismatch | invalid_signature | conflict | forbidden | invalid_slug
	Message string
}

func (e *PublishError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// PublishRequest is the validated publish input passed from the API handler
// to PublishRelease.
type PublishRequest struct {
	Slug           string
	Publisher      *Publisher
	TarballBytes   []byte
	SignatureBytes []byte
	Changelog      string
}

// PublishResult is returned on a successful publish.
type PublishResult struct {
	Release        *storage.Release
	ManifestDigest string // sha256 hex, no prefix
}

// PublishRelease runs the full publish pipeline:
//
//  1. Validate size + slug format.
//  2. SHA-256 the tarball (content-address).
//  3. Extract SKILL.md, parse it with the F09 schema validator.
//  4. Cross-check manifest name vs URL slug.
//  5. Verify the detached signature.
//  6. Write the blob, insert the release record.
func PublishRelease(
	ctx context.Context,
	req *PublishRequest,
	meta storage.MetadataStore,
	blobs storage.BlobStore,
	verifier SignatureVerifier,
	sigAlgorithm string,
) (*PublishResult, error) {
	if len(req.TarballBytes) == 0 {
		return nil, &PublishError{"invalid_tarball", "tarball is empty"}
	}
	if len(req.TarballBytes) > MaxTarballBytes {
		return nil, &PublishError{"invalid_tarball", fmt.Sprintf("tarball exceeds %d byte cap", MaxTarballBytes)}
	}
	if !slugRE.MatchString(req.Slug) {
		return nil, &PublishError{"invalid_slug", fmt.Sprintf("slug must be a-z0-9_-, got %q", req.Slug)}
	}

	digest := storage.SHA256Hex(req.TarballBytes)

	skillMDText, err := extractSkillMD(req.TarballBytes)
	if err != nil {
		return nil, err
	}

	manifest, parseErr := skills.ParseSkillMD(skillMDText, req.Slug)
	if parseErr != nil {
		return nil, &PublishError{"invalid_manifest", parseErr.Error()}
	}

	if manifest.Name != req.Slug {
		return nil, &PublishError{"slug_mismatch",
			fmt.Sprintf("SKILL.md 'name' is %q but URL slug is %q", manifest.Name, req.Slug)}
	}

	ok, verifyErr := verifier.Verify(req.TarballBytes, req.SignatureBytes, req.Publisher.PublicKeyPEM)
	if verifyErr != nil {
		return nil, &PublishError{"invalid_signature", "signature verification error: " + verifyErr.Error()}
	}
	if !ok {
		return nil, &PublishError{"invalid_signature", "signature does not verify against publisher key"}
	}

	if _, putErr := blobs.Put(req.TarballBytes); putErr != nil {
		return nil, &PublishError{"invalid_tarball", "blob store write failed: " + putErr.Error()}
	}

	mJSON := serializeManifest(manifest)
	sigB64 := base64.StdEncoding.EncodeToString(req.SignatureBytes)

	params := storage.InsertReleaseParams{
		Slug:               req.Slug,
		PublisherHandle:    req.Publisher.Handle,
		Version:            manifest.Version,
		ManifestDigest:     digest,
		BlobSizeBytes:      int64(len(req.TarballBytes)),
		ManifestJSON:       mJSON,
		Description:        strings.TrimSpace(manifest.Description),
		Summary:            strings.TrimSpace(manifest.Description),
		Tags:               manifest.AllowedTools,
		Changelog:          req.Changelog,
		SignatureB64:       sigB64,
		SignatureKeyID:     req.Publisher.Handle,
		SignatureAlgorithm: sigAlgorithm,
	}
	release, insErr := meta.InsertRelease(ctx, params)
	if insErr != nil {
		msg := insErr.Error()
		switch {
		case strings.Contains(msg, "already exists"):
			return nil, &PublishError{"conflict", msg}
		case strings.Contains(msg, "different publisher"):
			return nil, &PublishError{"forbidden", msg}
		case strings.Contains(msg, "unknown publisher"):
			return nil, &PublishError{"forbidden", msg}
		}
		return nil, &PublishError{"invalid_tarball", msg}
	}

	return &PublishResult{Release: release, ManifestDigest: digest}, nil
}

// extractSkillMD locates and reads SKILL.md from a gzipped tarball.
// Accepts SKILL.md at the archive root or one level deep (<slug>/SKILL.md).
func extractSkillMD(tarball []byte) (string, error) {
	tr, err := openTar(tarball)
	if err != nil {
		return "", &PublishError{"invalid_tarball", "tarball is corrupt: " + err.Error()}
	}

	type candidate struct {
		depth int
		data  []byte
	}
	var best *candidate

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", &PublishError{"invalid_tarball", "tarball read error: " + err.Error()}
		}
		name := hdr.Name
		// Safety: reject absolute paths and traversals.
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return "", &PublishError{"invalid_tarball", fmt.Sprintf("tarball contains unsafe path: %q", name)}
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if name != "SKILL.md" && !strings.HasSuffix(name, "/SKILL.md") {
			continue
		}
		depth := strings.Count(name, "/")
		if depth > 1 {
			return "", &PublishError{"invalid_tarball",
				fmt.Sprintf("SKILL.md must live at the archive root, found %q", name)}
		}
		data, readErr := io.ReadAll(tr)
		if readErr != nil {
			return "", &PublishError{"invalid_tarball", "could not read SKILL.md from archive"}
		}
		if best == nil || depth < best.depth {
			best = &candidate{depth: depth, data: data}
		}
	}

	if best == nil {
		return "", &PublishError{"invalid_tarball", "tarball does not contain a SKILL.md"}
	}
	return string(best.data), nil
}

// openTar opens a gzip-wrapped tar stream from payload.
// Falls back to raw tar if the payload is not gzip-compressed.
func openTar(payload []byte) (*tar.Reader, error) {
	gr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		// Try raw (uncompressed) tar.
		return tar.NewReader(bytes.NewReader(payload)), nil
	}
	return tar.NewReader(gr), nil
}

func serializeManifest(m *skills.SkillManifest) map[string]any {
	allowedTools := m.AllowedTools
	if allowedTools == nil {
		allowedTools = []string{}
	}
	requiredEnv := m.RequiredEnv
	if requiredEnv == nil {
		requiredEnv = []string{}
	}
	command := m.Mcp.Command
	if command == nil {
		command = []string{}
	}
	return map[string]any{
		"name":          m.Name,
		"version":       m.Version,
		"description":   m.Description,
		"when_to_use":   m.WhenToUse,
		"allowed_tools": allowedTools,
		"required_env":  requiredEnv,
		"mcp": map[string]any{
			"enabled":   m.Mcp.Enabled,
			"command":   command,
			"transport": m.Mcp.Transport,
		},
		"signing": map[string]any{
			"publisher": m.Signing.Publisher,
			"sig":       m.Signing.Sig,
		},
	}
}

// DecodeSignatureField accepts hex or base64 encoded text and returns raw bytes.
// Falls back to UTF-8 bytes; the verifier will reject a malformed signature.
func DecodeSignatureField(value string) []byte {
	text := strings.TrimSpace(value)
	// Try hex first: hex is a strict subset of base64, so we must check hex first.
	if isHex(text) {
		if b, err := hex.DecodeString(text); err == nil {
			return b
		}
	}
	if b, err := base64.StdEncoding.DecodeString(text); err == nil {
		return b
	}
	if b, err := base64.RawStdEncoding.DecodeString(text); err == nil {
		return b
	}
	return []byte(text)
}

func isHex(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
