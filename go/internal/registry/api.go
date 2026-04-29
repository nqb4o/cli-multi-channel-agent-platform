package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openclaw/agent-platform/internal/registry/storage"
)

// Deps bundles the collaborators wired into every request. Tests pass in
// in-memory fakes; production wires Postgres + filesystem blobs.
type Deps struct {
	Metadata         storage.MetadataStore
	Blobs            storage.BlobStore
	Verifier         SignatureVerifier
	SignatureAlgorithm string // "ed25519" | "sigstore-cosign" | "always-accept"
}

// NewDepsFromConfig builds production Deps from the resolved Config.
// An optional pgxpool.Pool is accepted so the caller can control pool
// lifecycle; pass nil and DSN non-empty to have NewDepsFromConfig open its
// own pool.
func NewDepsFromConfig(cfg Config, pool *pgxpool.Pool) (*Deps, error) {
	var blobs storage.BlobStore
	if cfg.BlobDir != "" {
		fsb, err := storage.NewFilesystemBlobStore(cfg.BlobDir)
		if err != nil {
			return nil, fmt.Errorf("blob store: %w", err)
		}
		blobs = fsb
	} else {
		blobs = storage.NewInMemoryBlobStore()
	}

	verifier, algo, err := MakeVerifier(cfg.VerifierKind, cfg.AllowInsecureVerifier)
	if err != nil {
		return nil, fmt.Errorf("verifier: %w", err)
	}

	var meta storage.MetadataStore
	if pool != nil {
		meta = storage.NewPostgresMetadataStore(pool)
	} else {
		meta = storage.NewInMemoryMetadataStore()
	}

	return &Deps{
		Metadata:           meta,
		Blobs:              blobs,
		Verifier:           verifier,
		SignatureAlgorithm: algo,
	}, nil
}

// NewRouter builds the chi router for the registry service.
//
// The factory deliberately does not read environment variables so tests
// can inject in-memory fakes and remain deterministic.
func NewRouter(deps *Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", handleHealthz())
	r.Get("/skills", handleSearch(deps))
	r.Get("/skills/{slug}", handlePackageDetail(deps))
	r.Get("/skills/{slug}/versions/{version}", handleReleaseDetail(deps))
	r.Get("/skills/{slug}/versions/{version}/tarball", handleDownloadTarball(deps))
	r.Get("/skills/{slug}/versions/{version}/sig", handleDownloadSignature(deps))
	r.Get("/keys/{publisher}", handlePublisherKey(deps))
	r.Post("/skills/{slug}/versions", handlePublishVersion(deps))

	return r
}

// ---------------------------------------------------------------------------
// Handler factories.
// ---------------------------------------------------------------------------

func handleHealthz() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func handleSearch(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		limit := 25
		if s := r.URL.Query().Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				limit = n
			}
		}
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
		pkgs, err := deps.Metadata.SearchPackages(r.Context(), q, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search_error", err.Error())
			return
		}
		results := make([]any, 0, len(pkgs))
		for _, p := range pkgs {
			results = append(results, packageDTO(p))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"results": results,
			"query":   q,
			"limit":   limit,
		})
	}
}

func handlePackageDetail(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		pkg, err := deps.Metadata.GetPackage(r.Context(), slug)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if pkg == nil {
			writeError(w, http.StatusNotFound, "not_found", "skill not found")
			return
		}
		releases, err := deps.Metadata.ListReleases(r.Context(), slug)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		versions := make([]any, 0, len(releases))
		for _, rel := range releases {
			versions = append(versions, releaseDTO(rel))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"package":  packageDTO(*pkg),
			"versions": versions,
		})
	}
}

func handleReleaseDetail(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		version := chi.URLParam(r, "version")
		rel, err := deps.Metadata.GetRelease(r.Context(), slug, version)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if rel == nil {
			writeError(w, http.StatusNotFound, "not_found", "release not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"release": releaseDTO(*rel)})
	}
}

func handleDownloadTarball(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		version := chi.URLParam(r, "version")
		rel, err := deps.Metadata.GetRelease(r.Context(), slug, version)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if rel == nil {
			writeError(w, http.StatusNotFound, "not_found", "release not found")
			return
		}
		data, err := deps.Blobs.Get(rel.ManifestDigest)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusGone, "blob_unavailable", "blob not available")
				return
			}
			log.Printf("registry: blob get error for %s@%s: %v", slug, version, err)
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:"+rel.ManifestDigest)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func handleDownloadSignature(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		version := chi.URLParam(r, "version")
		rel, err := deps.Metadata.GetRelease(r.Context(), slug, version)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if rel == nil {
			writeError(w, http.StatusNotFound, "not_found", "release not found")
			return
		}
		if rel.SignatureB64 == "" {
			writeError(w, http.StatusNotFound, "not_found", "release has no signature")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"signature_b64": rel.SignatureB64,
			"key_id":        rel.SignatureKeyID,
			"algorithm":     rel.SignatureAlgorithm,
		})
	}
}

func handlePublisherKey(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handle := chi.URLParam(r, "publisher")
		pub, err := deps.Metadata.GetPublisherByHandle(r.Context(), handle)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_error", err.Error())
			return
		}
		if pub == nil {
			writeError(w, http.StatusNotFound, "not_found", "publisher not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"handle":         pub.Handle,
			"display_name":   pub.DisplayName,
			"public_key_pem": pub.PublicKeyPEM,
		})
	}
}

func handlePublishVersion(deps *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")

		// Auth first — failed auth gets 401 before reading the body.
		publisher, authErr := RequirePublisher(r.Context(), r.Header.Get("Authorization"), deps.Metadata)
		if authErr != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", authErr.Error())
			return
		}

		tarball, sigBytes, changelog, parseErr := readPublishMultipart(r)
		if parseErr != nil {
			var pe *PublishError
			if errors.As(parseErr, &pe) {
				writeError(w, http.StatusBadRequest, pe.Code, pe.Message)
			} else {
				writeError(w, http.StatusBadRequest, "invalid_request", parseErr.Error())
			}
			return
		}

		req := &PublishRequest{
			Slug:           slug,
			Publisher:      publisher,
			TarballBytes:   tarball,
			SignatureBytes: sigBytes,
			Changelog:      changelog,
		}
		result, pubErr := PublishRelease(r.Context(), req, deps.Metadata, deps.Blobs, deps.Verifier, deps.SignatureAlgorithm)
		if pubErr != nil {
			var pe *PublishError
			if errors.As(pubErr, &pe) {
				status := publishErrorToStatus(pe.Code)
				writeError(w, status, pe.Code, pe.Message)
				return
			}
			writeError(w, http.StatusInternalServerError, "internal_error", pubErr.Error())
			return
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"release":         releaseDTO(*result.Release),
			"manifest_digest": "sha256:" + result.ManifestDigest,
		})
	}
}

// ---------------------------------------------------------------------------
// Multipart parser.
// ---------------------------------------------------------------------------

// readPublishMultipart parses the multipart body for POST /skills/{slug}/versions.
// Returns (tarball, signatureBytes, changelog, error).
// The signature field accepts raw bytes (octet-stream) or base64-encoded text.
func readPublishMultipart(r *http.Request) ([]byte, []byte, string, error) {
	if err := r.ParseMultipartForm(MaxTarballBytes + 1*1024*1024); err != nil {
		return nil, nil, "", &PublishError{"invalid_tarball", "could not parse multipart body: " + err.Error()}
	}
	form := r.MultipartForm
	if form == nil {
		return nil, nil, "", &PublishError{"invalid_tarball", "missing multipart form"}
	}

	// Tarball field.
	tarballFiles, ok := form.File["tarball"]
	if !ok || len(tarballFiles) == 0 {
		return nil, nil, "", &PublishError{"invalid_tarball", "missing 'tarball' multipart field"}
	}
	tf, err := tarballFiles[0].Open()
	if err != nil {
		return nil, nil, "", &PublishError{"invalid_tarball", "could not open tarball field: " + err.Error()}
	}
	defer tf.Close()
	tarball, err := io.ReadAll(tf)
	if err != nil {
		return nil, nil, "", &PublishError{"invalid_tarball", "could not read tarball: " + err.Error()}
	}

	// Signature field — may be a file part (octet-stream) or a text value.
	var sigBytes []byte
	if sigFiles, ok := form.File["signature"]; ok && len(sigFiles) > 0 {
		sf, err := sigFiles[0].Open()
		if err != nil {
			return nil, nil, "", &PublishError{"invalid_signature", "could not open signature field: " + err.Error()}
		}
		defer sf.Close()
		raw, err := io.ReadAll(sf)
		if err != nil {
			return nil, nil, "", &PublishError{"invalid_signature", "could not read signature: " + err.Error()}
		}
		// The CLI uploads base64(raw_sig) as octet-stream.
		sigBytes = DecodeSignatureField(string(raw))
	} else if sigValues, ok := form.Value["signature"]; ok && len(sigValues) > 0 {
		sigBytes = DecodeSignatureField(sigValues[0])
	} else {
		return nil, nil, "", &PublishError{"invalid_signature", "missing 'signature' multipart field"}
	}

	// Optional metadata field (JSON with "changelog" key).
	changelog := ""
	if mdValues, ok := form.Value["metadata"]; ok && len(mdValues) > 0 {
		mdRaw := strings.TrimSpace(mdValues[0])
		if mdRaw != "" {
			var mdObj map[string]any
			if err := json.Unmarshal([]byte(mdRaw), &mdObj); err != nil {
				return nil, nil, "", &PublishError{"invalid_manifest", "metadata is not valid JSON: " + err.Error()}
			}
			if cl, ok := mdObj["changelog"]; ok && cl != nil {
				switch v := cl.(type) {
				case string:
					changelog = v
				default:
					return nil, nil, "", &PublishError{"invalid_manifest", "metadata.changelog must be a string"}
				}
			}
		}
	}

	return tarball, sigBytes, changelog, nil
}

// ---------------------------------------------------------------------------
// DTO helpers.
// ---------------------------------------------------------------------------

func packageDTO(p storage.SkillPackage) map[string]any {
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"slug":           p.Slug,
		"publisher":      p.PublisherHandle,
		"description":    p.Description,
		"summary":        p.Summary,
		"tags":           tags,
		"latest_version": nullableString(p.LatestVersion),
		"created_at":     p.CreatedAt.Format(time.RFC3339Nano),
		"updated_at":     p.UpdatedAt.Format(time.RFC3339Nano),
	}
}

func releaseDTO(rel storage.Release) map[string]any {
	var sig any
	if rel.SignatureB64 != "" {
		sig = map[string]any{
			"key_id":    rel.SignatureKeyID,
			"algorithm": rel.SignatureAlgorithm,
		}
	}
	mJSON := rel.ManifestJSON
	if mJSON == nil {
		mJSON = map[string]any{}
	}
	return map[string]any{
		"slug":             rel.Slug,
		"version":          rel.Version,
		"manifest_digest":  "sha256:" + rel.ManifestDigest,
		"blob_size_bytes":  rel.BlobSizeBytes,
		"manifest":         mJSON,
		"changelog":        rel.Changelog,
		"yanked":           rel.Yanked,
		"created_at":       rel.CreatedAt.Format(time.RFC3339Nano),
		"signature":        sig,
	}
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func publishErrorToStatus(code string) int {
	switch code {
	case "forbidden":
		return http.StatusForbidden
	case "conflict":
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// ---------------------------------------------------------------------------
// JSON helpers.
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("registry: writeJSON encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

// Ensure base64 is used (for publish.go).
var _ = base64.StdEncoding
