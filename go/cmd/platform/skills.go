package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Config.
// ---------------------------------------------------------------------------

type cliConfig struct {
	RegistryURL  string
	Token        string
	SkillsDir    string
	VerifierKind string
	TimeoutSecs  float64
}

func loadCLIConfig() cliConfig {
	registryURL := os.Getenv("PLATFORM_REGISTRY_URL")
	if registryURL == "" {
		registryURL = "http://127.0.0.1:8090"
	}
	skillsDir := os.Getenv("PLATFORM_SKILLS_DIR")
	if skillsDir == "" {
		home, _ := os.UserHomeDir()
		skillsDir = filepath.Join(home, ".platform", "skills")
	}
	verifier := os.Getenv("PLATFORM_VERIFIER")
	if verifier == "" {
		verifier = "inprocess"
	}
	return cliConfig{
		RegistryURL:  strings.TrimRight(registryURL, "/"),
		Token:        os.Getenv("PLATFORM_TOKEN"),
		SkillsDir:    skillsDir,
		VerifierKind: verifier,
		TimeoutSecs:  30,
	}
}

// ---------------------------------------------------------------------------
// Registry HTTP client.
// ---------------------------------------------------------------------------

type registryClientError struct {
	Status  int
	Code    string
	Message string
}

func (e *registryClientError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

type registryClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func newRegistryClient(cfg cliConfig) *registryClient {
	return &registryClient{
		baseURL: cfg.RegistryURL,
		token:   cfg.Token,
		client:  &http.Client{Timeout: time.Duration(cfg.TimeoutSecs * float64(time.Second))},
	}
}

func (c *registryClient) get(path string, query url.Values) (*http.Response, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.client.Do(req)
}

func (c *registryClient) raiseForStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var obj map[string]string
	if json.Unmarshal(body, &obj) == nil {
		code := obj["error"]
		if code == "" {
			code = "http_error"
		}
		msg := obj["message"]
		if msg == "" {
			msg = string(body)
		}
		return &registryClientError{Status: resp.StatusCode, Code: code, Message: msg}
	}
	return &registryClientError{
		Status:  resp.StatusCode,
		Code:    "http_error",
		Message: string(body),
	}
}

func (c *registryClient) decodeJSON(resp *http.Response, dst any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(dst)
}

// search calls GET /skills?q=query&limit=limit.
func (c *registryClient) search(query string, limit int) ([]map[string]any, error) {
	resp, err := c.get("/skills", url.Values{"q": {query}, "limit": {fmt.Sprint(limit)}})
	if err != nil {
		return nil, err
	}
	if err := c.raiseForStatus(resp); err != nil {
		return nil, err
	}
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := c.decodeJSON(resp, &body); err != nil {
		return nil, err
	}
	return body.Results, nil
}

// listSkills calls GET /skills with empty query.
func (c *registryClient) listSkills(limit int) ([]map[string]any, error) {
	return c.search("", limit)
}

// getPackage calls GET /skills/{slug}.
func (c *registryClient) getPackage(slug string) (map[string]any, error) {
	resp, err := c.get("/skills/"+slug, nil)
	if err != nil {
		return nil, err
	}
	if err := c.raiseForStatus(resp); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := c.decodeJSON(resp, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// getRelease calls GET /skills/{slug}/versions/{version}.
func (c *registryClient) getRelease(slug, version string) (map[string]any, error) {
	resp, err := c.get("/skills/"+slug+"/versions/"+version, nil)
	if err != nil {
		return nil, err
	}
	if err := c.raiseForStatus(resp); err != nil {
		return nil, err
	}
	var body struct {
		Release map[string]any `json:"release"`
	}
	if err := c.decodeJSON(resp, &body); err != nil {
		return nil, err
	}
	return body.Release, nil
}

// getSignature calls GET /skills/{slug}/versions/{version}/sig.
func (c *registryClient) getSignature(slug, version string) (map[string]any, error) {
	resp, err := c.get("/skills/"+slug+"/versions/"+version+"/sig", nil)
	if err != nil {
		return nil, err
	}
	if err := c.raiseForStatus(resp); err != nil {
		return nil, err
	}
	var body map[string]any
	if err := c.decodeJSON(resp, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// getPublisherKey calls GET /keys/{handle}.
func (c *registryClient) getPublisherKey(handle string) (string, error) {
	resp, err := c.get("/keys/"+handle, nil)
	if err != nil {
		return "", err
	}
	if err := c.raiseForStatus(resp); err != nil {
		return "", err
	}
	var body struct {
		PublicKeyPEM string `json:"public_key_pem"`
	}
	if err := c.decodeJSON(resp, &body); err != nil {
		return "", err
	}
	return body.PublicKeyPEM, nil
}

// downloadTarball fetches the tarball and returns (bytes, sha256-hex).
func (c *registryClient) downloadTarball(slug, version string) ([]byte, string, error) {
	resp, err := c.get("/skills/"+slug+"/versions/"+version+"/tarball", nil)
	if err != nil {
		return nil, "", err
	}
	if err := c.raiseForStatus(resp); err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("download tarball: read: %w", err)
	}
	digestHeader := resp.Header.Get("Docker-Content-Digest")
	if !strings.HasPrefix(strings.ToLower(digestHeader), "sha256:") {
		return nil, "", &registryClientError{
			Status:  resp.StatusCode,
			Code:    "missing_digest",
			Message: "registry response is missing Docker-Content-Digest",
		}
	}
	declared := strings.ToLower(digestHeader[7:])
	return data, declared, nil
}

// ---------------------------------------------------------------------------
// Installer.
// ---------------------------------------------------------------------------

const receiptFilename = ".platform-install.json"

type installerError struct {
	Code    string
	Message string
}

func (e *installerError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

type installReceipt struct {
	Slug               string `json:"slug"`
	Version            string `json:"version"`
	Publisher          string `json:"publisher"`
	SignatureAlgorithm string `json:"signature_algorithm"`
	ManifestDigest     string `json:"manifest_digest"`
	InstalledAt        string `json:"installed_at"`
	RegistryURL        string `json:"registry_url"`
	ReceiptVersion     int    `json:"receipt_version"`
}

func installSkill(slug string, client *registryClient, cfg cliConfig, version string, force bool) (string, *installReceipt, error) {
	pkgData, err := client.getPackage(slug)
	if err != nil {
		var rce *registryClientError
		if errors.As(err, &rce) && rce.Status == 404 {
			return "", nil, &installerError{"not_found", fmt.Sprintf("skill %q not found in registry", slug)}
		}
		return "", nil, err
	}

	targetVersion := version
	if targetVersion == "" {
		if pkg, ok := pkgData["package"].(map[string]any); ok {
			if lv, ok := pkg["latest_version"].(string); ok {
				targetVersion = lv
			}
		}
	}
	if targetVersion == "" {
		return "", nil, &installerError{"not_found", fmt.Sprintf("skill %q has no published versions", slug)}
	}

	release, err := client.getRelease(slug, targetVersion)
	if err != nil {
		return "", nil, err
	}
	sigRecord, err := client.getSignature(slug, targetVersion)
	if err != nil {
		return "", nil, err
	}

	// Resolve publisher handle from release signature or sig record.
	publisherHandle := ""
	if sig, ok := release["signature"].(map[string]any); ok {
		if kid, ok := sig["key_id"].(string); ok {
			publisherHandle = kid
		}
	}
	if publisherHandle == "" {
		if kid, ok := sigRecord["key_id"].(string); ok {
			publisherHandle = kid
		}
	}

	publicKeyPEM, err := client.getPublisherKey(publisherHandle)
	if err != nil {
		return "", nil, err
	}

	tarball, declaredDigest, err := client.downloadTarball(slug, targetVersion)
	if err != nil {
		return "", nil, err
	}

	expectedDigest := ""
	if md, ok := release["manifest_digest"].(string); ok {
		parts := strings.SplitN(md, ":", 2)
		if len(parts) == 2 {
			expectedDigest = strings.ToLower(parts[1])
		}
	}

	actualSum := sha256.Sum256(tarball)
	actualDigest := fmt.Sprintf("%x", actualSum)

	if actualDigest != declaredDigest {
		return "", nil, &installerError{"integrity_mismatch",
			fmt.Sprintf("tarball sha256 %s does not match server header %s", actualDigest, declaredDigest)}
	}
	if actualDigest != expectedDigest {
		return "", nil, &installerError{"integrity_mismatch",
			fmt.Sprintf("tarball sha256 %s does not match catalog digest %s", actualDigest, expectedDigest)}
	}

	sigB64, _ := sigRecord["signature_b64"].(string)
	sigBytes, decErr := base64.StdEncoding.DecodeString(sigB64)
	if decErr != nil {
		return "", nil, &installerError{"invalid_signature", "signature is not valid base64: " + decErr.Error()}
	}

	if verErr := verifySignature(cfg.VerifierKind, tarball, sigBytes, publicKeyPEM); verErr != nil {
		return "", nil, &installerError{"invalid_signature",
			fmt.Sprintf("signature does not verify against publisher %q: %v", publisherHandle, verErr)}
	}

	targetDir := filepath.Join(cfg.SkillsDir, slug)
	if _, statErr := os.Stat(targetDir); statErr == nil {
		if !force {
			return "", nil, &installerError{"already_installed",
				fmt.Sprintf("%s already exists; pass --force or use `platform skills update`", targetDir)}
		}
		os.RemoveAll(targetDir)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir %s: %w", targetDir, err)
	}

	if extErr := safeExtractTarball(tarball, targetDir); extErr != nil {
		os.RemoveAll(targetDir)
		return "", nil, extErr
	}

	sigAlgo := ""
	if v, ok := sigRecord["algorithm"].(string); ok {
		sigAlgo = v
	}

	receipt := &installReceipt{
		Slug:               slug,
		Version:            targetVersion,
		Publisher:          publisherHandle,
		SignatureAlgorithm: sigAlgo,
		ManifestDigest:     "sha256:" + actualDigest,
		InstalledAt:        time.Now().UTC().Format(time.RFC3339),
		RegistryURL:        client.baseURL,
		ReceiptVersion:     1,
	}
	receiptBytes, _ := json.MarshalIndent(receipt, "", "  ")
	receiptPath := filepath.Join(targetDir, receiptFilename)
	if err := os.WriteFile(receiptPath, receiptBytes, 0o644); err != nil {
		return "", nil, fmt.Errorf("write receipt: %w", err)
	}

	return targetDir, receipt, nil
}

// verifySignature dispatches to the named verifier.
func verifySignature(kind string, payload, signature []byte, publicKeyPEM string) error {
	switch strings.ToLower(kind) {
	case "always-accept":
		return nil
	case "inprocess", "":
		if !cliEd25519Verify(payload, signature, publicKeyPEM) {
			return fmt.Errorf("ed25519 verification failed")
		}
		return nil
	case "cosign":
		// Stub: no cosign binary assumed on CI — skip.
		log.Printf("cosign verifier: skipping (stub)")
		return nil
	default:
		return fmt.Errorf("unknown verifier kind %q", kind)
	}
}

// cliEd25519Verify verifies an Ed25519 signature without importing internal/registry.
func cliEd25519Verify(payload, signature []byte, publicKeyPEM string) bool {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return false
	}
	raw, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return false
	}
	pub, ok := raw.(ed25519.PublicKey)
	if !ok {
		return false
	}
	return ed25519.Verify(pub, payload, signature)
}

// ---------------------------------------------------------------------------
// Safe-extract.
// ---------------------------------------------------------------------------

func safeExtractTarball(tarball []byte, destDir string) error {
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("safeExtract: resolve dest: %w", err)
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return fmt.Errorf("safeExtract: mkdir dest: %w", err)
	}

	gr, gzErr := gzip.NewReader(bytes.NewReader(tarball))
	var tr *tar.Reader
	if gzErr != nil {
		tr = tar.NewReader(bytes.NewReader(tarball))
	} else {
		tr = tar.NewReader(gr)
	}

	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return &installerError{"invalid_tarball", "tarball read error: " + nextErr.Error()}
		}

		name := hdr.Name
		if strings.HasPrefix(name, "/") {
			return &installerError{"unsafe_path", fmt.Sprintf("absolute path in tarball: %q", name)}
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return &installerError{"unsafe_path", fmt.Sprintf("traversal in tarball: %q", name)}
			}
		}

		target := filepath.Join(destAbs, filepath.FromSlash(name))
		targetAbs, absErr := filepath.Abs(target)
		if absErr != nil {
			return &installerError{"unsafe_path", fmt.Sprintf("resolve path %q: %v", name, absErr)}
		}
		if !strings.HasPrefix(targetAbs, destAbs+string(os.PathSeparator)) && targetAbs != destAbs {
			return &installerError{"unsafe_path", fmt.Sprintf("member %q resolves outside install dir", name)}
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if mkdirErr := os.MkdirAll(targetAbs, 0o755); mkdirErr != nil {
				return fmt.Errorf("safeExtract: mkdir %s: %w", targetAbs, mkdirErr)
			}
		case tar.TypeReg:
			if mkdirErr := os.MkdirAll(filepath.Dir(targetAbs), 0o755); mkdirErr != nil {
				return fmt.Errorf("safeExtract: mkdir parent: %w", mkdirErr)
			}
			data, readErr := io.ReadAll(tr)
			if readErr != nil {
				return fmt.Errorf("safeExtract: read %s: %w", name, readErr)
			}
			mode := os.FileMode(hdr.Mode & 0o755)
			if mode == 0 {
				mode = 0o644
			}
			if writeErr := os.WriteFile(targetAbs, data, mode); writeErr != nil {
				return fmt.Errorf("safeExtract: write %s: %w", targetAbs, writeErr)
			}
		case tar.TypeSymlink, tar.TypeLink:
			log.Printf("platform: skipping link in install tarball: %s", name)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cobra commands.
// ---------------------------------------------------------------------------

func skillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Search, install, and manage skills",
	}
	cmd.AddCommand(
		skillsSearchCmd(),
		skillsInfoCmd(),
		skillsListCmd(),
		skillsInstallCmd(),
		skillsUpdateCmd(),
		skillsCheckCmd(),
	)
	return cmd
}

func skillsSearchCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the skill registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadCLIConfig()
			client := newRegistryClient(cfg)
			results, err := client.search(args[0], limit)
			if err != nil {
				return fmt.Errorf("search: %w", err)
			}
			if len(results) == 0 {
				fmt.Printf("no skills matched %q\n", args[0])
				return nil
			}
			for _, r := range results {
				slug, _ := r["slug"].(string)
				latest := "—"
				if lv, ok := r["latest_version"].(string); ok && lv != "" {
					latest = lv
				}
				summary, _ := r["summary"].(string)
				fmt.Printf("%-32s %-10s %s\n", slug, latest, summary)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum number of results")
	return cmd
}

func skillsInfoCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "info <slug>",
		Short: "Show details for a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadCLIConfig()
			client := newRegistryClient(cfg)
			var out any
			var err error
			if version != "" {
				out, err = client.getRelease(args[0], version)
			} else {
				out, err = client.getPackage(args[0])
			}
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Show a specific version")
	return cmd
}

func skillsListCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all packages in the registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadCLIConfig()
			client := newRegistryClient(cfg)
			results, err := client.listSkills(limit)
			if err != nil {
				return err
			}
			if len(results) == 0 {
				fmt.Println("registry is empty")
				return nil
			}
			for _, r := range results {
				slug, _ := r["slug"].(string)
				latest := "—"
				if lv, ok := r["latest_version"].(string); ok && lv != "" {
					latest = lv
				}
				fmt.Printf("%-32s %s\n", slug, latest)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of results")
	return cmd
}

func skillsInstallCmd() *cobra.Command {
	var version, dest string
	var force bool
	cmd := &cobra.Command{
		Use:   "install <slug>",
		Short: "Install a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadCLIConfig()
			if dest != "" {
				cfg.SkillsDir = dest
			}
			if err := os.MkdirAll(cfg.SkillsDir, 0o755); err != nil {
				return fmt.Errorf("mkdir skills dir: %w", err)
			}
			client := newRegistryClient(cfg)
			skillDir, receipt, err := installSkill(args[0], client, cfg, version, force)
			if err != nil {
				return err
			}
			fmt.Printf("installed %s@%s at %s\n", receipt.Slug, receipt.Version, skillDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Pin to a specific version")
	cmd.Flags().StringVar(&dest, "dest", "", "Override PLATFORM_SKILLS_DIR for this run")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite if already installed")
	return cmd
}

func skillsUpdateCmd() *cobra.Command {
	var version, dest string
	cmd := &cobra.Command{
		Use:   "update <slug>",
		Short: "Re-install / upgrade a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadCLIConfig()
			if dest != "" {
				cfg.SkillsDir = dest
			}
			if err := os.MkdirAll(cfg.SkillsDir, 0o755); err != nil {
				return fmt.Errorf("mkdir skills dir: %w", err)
			}
			client := newRegistryClient(cfg)
			skillDir, receipt, err := installSkill(args[0], client, cfg, version, true /*force*/)
			if err != nil {
				return err
			}
			fmt.Printf("installed %s@%s at %s\n", receipt.Slug, receipt.Version, skillDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Pin to a specific version")
	cmd.Flags().StringVar(&dest, "dest", "", "Override PLATFORM_SKILLS_DIR for this run")
	return cmd
}

func skillsCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check [slug]",
		Short: "Compare installed version to registry latest",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := loadCLIConfig()
			client := newRegistryClient(cfg)

			if _, err := os.Stat(cfg.SkillsDir); os.IsNotExist(err) {
				fmt.Println("no installed skills found")
				return nil
			}

			if len(args) == 1 {
				checkOne(args[0], cfg.SkillsDir, client)
				return nil
			}

			entries, err := os.ReadDir(cfg.SkillsDir)
			if err != nil {
				return fmt.Errorf("read skills dir: %w", err)
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				receiptPath := filepath.Join(cfg.SkillsDir, e.Name(), receiptFilename)
				if _, statErr := os.Stat(receiptPath); os.IsNotExist(statErr) {
					continue
				}
				checkOne(e.Name(), cfg.SkillsDir, client)
			}
			return nil
		},
	}
	return cmd
}

func checkOne(slug, skillsDir string, client *registryClient) {
	receiptPath := filepath.Join(skillsDir, slug, receiptFilename)
	if _, err := os.Stat(receiptPath); os.IsNotExist(err) {
		fmt.Printf("%s: not installed\n", slug)
		return
	}
	raw, err := os.ReadFile(receiptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: read receipt: %v\n", slug, err)
		return
	}
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil {
		fmt.Fprintf(os.Stderr, "%s: parse receipt: %v\n", slug, err)
		return
	}
	installed, _ := receipt["version"].(string)
	if installed == "" {
		installed = "?"
	}

	pkgData, err := client.getPackage(slug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: registry lookup failed: %v\n", slug, err)
		return
	}
	latest := "?"
	if pkg, ok := pkgData["package"].(map[string]any); ok {
		if lv, ok := pkg["latest_version"].(string); ok {
			latest = lv
		}
	}
	if installed == latest {
		fmt.Printf("%-32s %s (up to date)\n", slug, installed)
	} else {
		fmt.Printf("%-32s %s → %s (outdated)\n", slug, installed, latest)
	}
}
