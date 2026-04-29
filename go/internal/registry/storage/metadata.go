package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Domain types.
// ---------------------------------------------------------------------------

// SkillPackage is the catalog row for a skill (slug + publisher + summary).
type SkillPackage struct {
	ID              uuid.UUID
	Slug            string
	PublisherHandle string
	Description     string
	Summary         string
	Tags            []string
	LatestVersion   string // empty if no releases
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Release is a single (slug, version) row with its signature info.
type Release struct {
	ID                 uuid.UUID
	PackageID          uuid.UUID
	Slug               string
	Version            string
	ManifestDigest     string // sha256-hex (no prefix)
	BlobSizeBytes      int64
	ManifestJSON       map[string]any
	Changelog          string
	Yanked             bool
	CreatedAt          time.Time
	SignatureB64       string
	SignatureKeyID     string
	SignatureAlgorithm string
}

// PublisherRow mirrors skill_publishers.
type PublisherRow struct {
	ID                  uuid.UUID
	Handle              string
	DisplayName         string
	PublicKeyPEM        string
	PublishTokenSHA256  string
}

// ---------------------------------------------------------------------------
// MetadataStore interface.
// ---------------------------------------------------------------------------

// MetadataStore is the catalog backend. Both the in-memory and Postgres
// variants satisfy this interface.
type MetadataStore interface {
	GetPublisherByTokenHash(ctx context.Context, tokenSHA256 string) (*PublisherRow, error)
	GetPublisherByHandle(ctx context.Context, handle string) (*PublisherRow, error)
	UpsertPublisher(ctx context.Context, handle, displayName, publicKeyPEM, publishTokenSHA256 string) (*PublisherRow, error)

	SearchPackages(ctx context.Context, query string, limit int) ([]SkillPackage, error)
	GetPackage(ctx context.Context, slug string) (*SkillPackage, error)
	ListReleases(ctx context.Context, slug string) ([]Release, error)
	GetRelease(ctx context.Context, slug, version string) (*Release, error)
	InsertRelease(ctx context.Context, p InsertReleaseParams) (*Release, error)
}

// InsertReleaseParams bundles the write fields for insert_release.
type InsertReleaseParams struct {
	Slug               string
	PublisherHandle    string
	Version            string
	ManifestDigest     string
	BlobSizeBytes      int64
	ManifestJSON       map[string]any
	Description        string
	Summary            string
	Tags               []string
	Changelog          string
	SignatureB64       string
	SignatureKeyID     string
	SignatureAlgorithm string
}

// ---------------------------------------------------------------------------
// In-memory store.
// ---------------------------------------------------------------------------

type memPackage struct {
	id          uuid.UUID
	slug        string
	publisherID uuid.UUID
	description string
	summary     string
	tags        []string
	createdAt   time.Time
	updatedAt   time.Time
	releases    []Release
}

// InMemoryMetadataStore is a thread-safe, dict-backed MetadataStore used by
// tests and laptop dev mode.
type InMemoryMetadataStore struct {
	mu             sync.Mutex
	byHandle       map[string]*PublisherRow
	byToken        map[string]*PublisherRow
	packages       map[string]*memPackage
}

// NewInMemoryMetadataStore constructs an empty in-memory store.
func NewInMemoryMetadataStore() *InMemoryMetadataStore {
	return &InMemoryMetadataStore{
		byHandle: make(map[string]*PublisherRow),
		byToken:  make(map[string]*PublisherRow),
		packages: make(map[string]*memPackage),
	}
}

func (s *InMemoryMetadataStore) GetPublisherByTokenHash(_ context.Context, tokenSHA256 string) (*PublisherRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.byToken[tokenSHA256]
	return row, nil
}

func (s *InMemoryMetadataStore) GetPublisherByHandle(_ context.Context, handle string) (*PublisherRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.byHandle[handle]
	return row, nil
}

func (s *InMemoryMetadataStore) UpsertPublisher(_ context.Context, handle, displayName, publicKeyPEM, publishTokenSHA256 string) (*PublisherRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.byHandle[handle]
	id := uuid.New()
	if existing != nil {
		id = existing.ID
		// Drop old token mapping.
		if existing.PublishTokenSHA256 != "" {
			delete(s.byToken, existing.PublishTokenSHA256)
		}
	}
	row := &PublisherRow{
		ID:                 id,
		Handle:             handle,
		DisplayName:        displayName,
		PublicKeyPEM:       publicKeyPEM,
		PublishTokenSHA256: publishTokenSHA256,
	}
	s.byHandle[handle] = row
	if publishTokenSHA256 != "" {
		s.byToken[publishTokenSHA256] = row
	}
	return row, nil
}

func (s *InMemoryMetadataStore) SearchPackages(_ context.Context, query string, limit int) ([]SkillPackage, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	s.mu.Lock()
	defer s.mu.Unlock()
	type scored struct {
		score int
		pkg   *memPackage
	}
	var results []scored
	for _, pkg := range s.packages {
		score := 0
		if q == "" {
			score = 1
		} else {
			score = scorePackage(pkg, q)
		}
		if score > 0 {
			results = append(results, scored{score, pkg})
		}
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].pkg.slug < results[j].pkg.slug
	})
	if limit > 0 && len(results) > limit {
		results = results[:limit]
	}
	out := make([]SkillPackage, 0, len(results))
	for _, r := range results {
		out = append(out, s.snapshot(r.pkg))
	}
	return out, nil
}

func (s *InMemoryMetadataStore) GetPackage(_ context.Context, slug string) (*SkillPackage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pkg := s.packages[slug]
	if pkg == nil {
		return nil, nil
	}
	snap := s.snapshot(pkg)
	return &snap, nil
}

func (s *InMemoryMetadataStore) ListReleases(_ context.Context, slug string) ([]Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pkg := s.packages[slug]
	if pkg == nil {
		return nil, nil
	}
	out := make([]Release, len(pkg.releases))
	copy(out, pkg.releases)
	return out, nil
}

func (s *InMemoryMetadataStore) GetRelease(_ context.Context, slug, version string) (*Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pkg := s.packages[slug]
	if pkg == nil {
		return nil, nil
	}
	for i := range pkg.releases {
		if pkg.releases[i].Version == version {
			r := pkg.releases[i]
			return &r, nil
		}
	}
	return nil, nil
}

func (s *InMemoryMetadataStore) InsertRelease(_ context.Context, p InsertReleaseParams) (*Release, error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()

	publisher := s.byHandle[p.PublisherHandle]
	if publisher == nil {
		return nil, fmt.Errorf("unknown publisher: %s", p.PublisherHandle)
	}
	pkg := s.packages[p.Slug]
	if pkg == nil {
		pkg = &memPackage{
			id:          uuid.New(),
			slug:        p.Slug,
			publisherID: publisher.ID,
			description: p.Description,
			summary:     p.Summary,
			tags:        p.Tags,
			createdAt:   now,
			updatedAt:   now,
		}
		s.packages[p.Slug] = pkg
	} else {
		if pkg.publisherID != publisher.ID {
			return nil, fmt.Errorf("package %q is owned by a different publisher", p.Slug)
		}
		for _, r := range pkg.releases {
			if r.Version == p.Version {
				return nil, fmt.Errorf("version %q already exists for %q", p.Version, p.Slug)
			}
		}
		if p.Description != "" {
			pkg.description = p.Description
		}
		if p.Summary != "" {
			pkg.summary = p.Summary
		}
		if len(p.Tags) > 0 {
			pkg.tags = p.Tags
		}
		pkg.updatedAt = now
	}
	mJSON := make(map[string]any)
	for k, v := range p.ManifestJSON {
		mJSON[k] = v
	}
	rel := Release{
		ID:                 uuid.New(),
		PackageID:          pkg.id,
		Slug:               p.Slug,
		Version:            p.Version,
		ManifestDigest:     p.ManifestDigest,
		BlobSizeBytes:      p.BlobSizeBytes,
		ManifestJSON:       mJSON,
		Changelog:          p.Changelog,
		Yanked:             false,
		CreatedAt:          now,
		SignatureB64:       p.SignatureB64,
		SignatureKeyID:     p.SignatureKeyID,
		SignatureAlgorithm: p.SignatureAlgorithm,
	}
	pkg.releases = append(pkg.releases, rel)
	sort.Slice(pkg.releases, func(i, j int) bool {
		return pkg.releases[i].CreatedAt.Before(pkg.releases[j].CreatedAt)
	})
	return &rel, nil
}

func (s *InMemoryMetadataStore) snapshot(pkg *memPackage) SkillPackage {
	var latest string
	if len(pkg.releases) > 0 {
		latest = pkg.releases[len(pkg.releases)-1].Version
	}
	handle := ""
	for _, p := range s.byHandle {
		if p.ID == pkg.publisherID {
			handle = p.Handle
			break
		}
	}
	tags := make([]string, len(pkg.tags))
	copy(tags, pkg.tags)
	return SkillPackage{
		ID:              pkg.id,
		Slug:            pkg.slug,
		PublisherHandle: handle,
		Description:     pkg.description,
		Summary:         pkg.summary,
		Tags:            tags,
		LatestVersion:   latest,
		CreatedAt:       pkg.createdAt,
		UpdatedAt:       pkg.updatedAt,
	}
}

func scorePackage(pkg *memPackage, q string) int {
	slug := strings.ToLower(pkg.slug)
	summary := strings.ToLower(pkg.summary)
	description := strings.ToLower(pkg.description)
	switch {
	case slug == q:
		return 100
	case strings.HasPrefix(slug, q):
		return 50
	case strings.Contains(slug, q):
		return 30
	}
	for _, t := range pkg.tags {
		if strings.ToLower(t) == q {
			return 20
		}
	}
	if strings.Contains(summary, q) {
		return 10
	}
	if strings.Contains(description, q) {
		return 5
	}
	return 0
}

// ---------------------------------------------------------------------------
// Postgres store.
// ---------------------------------------------------------------------------

// PostgresMetadataStore implements MetadataStore via pgx.
type PostgresMetadataStore struct {
	pool *pgxpool.Pool
}

// NewPostgresMetadataStore wraps an existing pgxpool.
func NewPostgresMetadataStore(pool *pgxpool.Pool) *PostgresMetadataStore {
	return &PostgresMetadataStore{pool: pool}
}

func (s *PostgresMetadataStore) GetPublisherByTokenHash(ctx context.Context, tokenSHA256 string) (*PublisherRow, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, handle, COALESCE(display_name,''), public_key_pem, COALESCE(publish_token_sha256,'')
		 FROM skill_publishers WHERE publish_token_sha256 = $1`,
		tokenSHA256,
	)
	return scanPublisher(row)
}

func (s *PostgresMetadataStore) GetPublisherByHandle(ctx context.Context, handle string) (*PublisherRow, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, handle, COALESCE(display_name,''), public_key_pem, COALESCE(publish_token_sha256,'')
		 FROM skill_publishers WHERE handle = $1`,
		handle,
	)
	return scanPublisher(row)
}

func (s *PostgresMetadataStore) UpsertPublisher(ctx context.Context, handle, displayName, publicKeyPEM, publishTokenSHA256 string) (*PublisherRow, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO skill_publishers (handle, display_name, public_key_pem, publish_token_sha256)
		 VALUES ($1, NULLIF($2,''), $3, NULLIF($4,''))
		 ON CONFLICT (handle) DO UPDATE SET
		     display_name       = EXCLUDED.display_name,
		     public_key_pem     = EXCLUDED.public_key_pem,
		     publish_token_sha256 = COALESCE(EXCLUDED.publish_token_sha256, skill_publishers.publish_token_sha256)
		 RETURNING id, handle, COALESCE(display_name,''), public_key_pem, COALESCE(publish_token_sha256,'')`,
		handle, displayName, publicKeyPEM, publishTokenSHA256,
	)
	return scanPublisher(row)
}

func (s *PostgresMetadataStore) SearchPackages(ctx context.Context, query string, limit int) ([]SkillPackage, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	var rows pgx.Rows
	var err error
	if q == "" {
		rows, err = s.pool.Query(ctx,
			`SELECT p.id, p.slug, pub.handle, p.description, p.summary,
			        p.tags, p.created_at, p.updated_at,
			        (SELECT version FROM skill_releases r
			         WHERE r.package_id = p.id ORDER BY r.created_at DESC LIMIT 1) AS latest_version
			 FROM skill_packages p
			 JOIN skill_publishers pub ON pub.id = p.publisher_id
			 ORDER BY p.slug LIMIT $1`,
			limit,
		)
	} else {
		like := "%" + q + "%"
		rows, err = s.pool.Query(ctx,
			`SELECT p.id, p.slug, pub.handle, p.description, p.summary,
			        p.tags, p.created_at, p.updated_at,
			        (SELECT version FROM skill_releases r
			         WHERE r.package_id = p.id ORDER BY r.created_at DESC LIMIT 1) AS latest_version
			 FROM skill_packages p
			 JOIN skill_publishers pub ON pub.id = p.publisher_id
			 WHERE LOWER(p.slug) LIKE $1
			    OR LOWER(p.summary) LIKE $1
			    OR LOWER(p.description) LIKE $1
			    OR EXISTS (SELECT 1 FROM unnest(p.tags) t WHERE LOWER(t) = $2)
			 ORDER BY
			     (CASE WHEN LOWER(p.slug) = $2 THEN 0
			           WHEN LOWER(p.slug) LIKE $3 THEN 1
			           ELSE 2 END),
			     p.slug
			 LIMIT $4`,
			like, q, q+"%", limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("SearchPackages: %w", err)
	}
	defer rows.Close()
	var out []SkillPackage
	for rows.Next() {
		var pkg SkillPackage
		var latest *string
		var tags []string
		if err := rows.Scan(&pkg.ID, &pkg.Slug, &pkg.PublisherHandle,
			&pkg.Description, &pkg.Summary, &tags,
			&pkg.CreatedAt, &pkg.UpdatedAt, &latest); err != nil {
			return nil, fmt.Errorf("SearchPackages scan: %w", err)
		}
		pkg.Tags = tags
		if latest != nil {
			pkg.LatestVersion = *latest
		}
		out = append(out, pkg)
	}
	return out, rows.Err()
}

func (s *PostgresMetadataStore) GetPackage(ctx context.Context, slug string) (*SkillPackage, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT p.id, p.slug, pub.handle, p.description, p.summary,
		        p.tags, p.created_at, p.updated_at,
		        (SELECT version FROM skill_releases r
		         WHERE r.package_id = p.id ORDER BY r.created_at DESC LIMIT 1) AS latest_version
		 FROM skill_packages p
		 JOIN skill_publishers pub ON pub.id = p.publisher_id
		 WHERE p.slug = $1`,
		slug,
	)
	var pkg SkillPackage
	var latest *string
	var tags []string
	if err := row.Scan(&pkg.ID, &pkg.Slug, &pkg.PublisherHandle,
		&pkg.Description, &pkg.Summary, &tags,
		&pkg.CreatedAt, &pkg.UpdatedAt, &latest); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetPackage: %w", err)
	}
	pkg.Tags = tags
	if latest != nil {
		pkg.LatestVersion = *latest
	}
	return &pkg, nil
}

func (s *PostgresMetadataStore) ListReleases(ctx context.Context, slug string) ([]Release, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, r.package_id, p.slug, r.version, r.manifest_digest,
		        r.blob_size_bytes, r.manifest_json::text, r.changelog, r.yanked, r.created_at,
		        COALESCE(sig.signature_b64,''), COALESCE(sig.key_id,''), COALESCE(sig.algorithm,'')
		 FROM skill_releases r
		 JOIN skill_packages p ON p.id = r.package_id
		 LEFT JOIN skill_signatures sig ON sig.release_id = r.id
		 WHERE p.slug = $1
		 ORDER BY r.created_at`,
		slug,
	)
	if err != nil {
		return nil, fmt.Errorf("ListReleases: %w", err)
	}
	defer rows.Close()
	return scanReleases(rows)
}

func (s *PostgresMetadataStore) GetRelease(ctx context.Context, slug, version string) (*Release, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT r.id, r.package_id, p.slug, r.version, r.manifest_digest,
		        r.blob_size_bytes, r.manifest_json::text, r.changelog, r.yanked, r.created_at,
		        COALESCE(sig.signature_b64,''), COALESCE(sig.key_id,''), COALESCE(sig.algorithm,'')
		 FROM skill_releases r
		 JOIN skill_packages p ON p.id = r.package_id
		 LEFT JOIN skill_signatures sig ON sig.release_id = r.id
		 WHERE p.slug = $1 AND r.version = $2`,
		slug, version,
	)
	var rel Release
	var mJSONText string
	if err := row.Scan(
		&rel.ID, &rel.PackageID, &rel.Slug, &rel.Version, &rel.ManifestDigest,
		&rel.BlobSizeBytes, &mJSONText, &rel.Changelog, &rel.Yanked, &rel.CreatedAt,
		&rel.SignatureB64, &rel.SignatureKeyID, &rel.SignatureAlgorithm,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetRelease: %w", err)
	}
	rel.ManifestJSON = coerceJSON(mJSONText)
	return &rel, nil
}

func (s *PostgresMetadataStore) InsertRelease(ctx context.Context, p InsertReleaseParams) (*Release, error) {
	mJSONBytes, err := json.Marshal(p.ManifestJSON)
	if err != nil {
		return nil, fmt.Errorf("InsertRelease: marshal manifest_json: %w", err)
	}
	mJSONText := string(mJSONBytes)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("InsertRelease: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var publisherID uuid.UUID
	if err := tx.QueryRow(ctx,
		"SELECT id FROM skill_publishers WHERE handle = $1",
		p.PublisherHandle,
	).Scan(&publisherID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("unknown publisher: %s", p.PublisherHandle)
		}
		return nil, fmt.Errorf("InsertRelease: lookup publisher: %w", err)
	}

	// Upsert the package row.
	var pkgID, actualPublisherID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO skill_packages (slug, publisher_id, description, summary, tags)
		 VALUES ($1, $2, $3, $4, $5::text[])
		 ON CONFLICT (slug) DO UPDATE SET
		     description = COALESCE(NULLIF(EXCLUDED.description,''), skill_packages.description),
		     summary     = COALESCE(NULLIF(EXCLUDED.summary,''), skill_packages.summary),
		     tags        = CASE WHEN array_length(EXCLUDED.tags,1) > 0 THEN EXCLUDED.tags ELSE skill_packages.tags END,
		     updated_at  = now()
		 RETURNING id, publisher_id`,
		p.Slug, publisherID, p.Description, p.Summary, p.Tags,
	).Scan(&pkgID, &actualPublisherID); err != nil {
		return nil, fmt.Errorf("InsertRelease: upsert package: %w", err)
	}
	if actualPublisherID != publisherID {
		return nil, fmt.Errorf("package %q is owned by a different publisher", p.Slug)
	}

	var rel Release
	var mText string
	if err := tx.QueryRow(ctx,
		`INSERT INTO skill_releases (package_id, version, manifest_digest, blob_size_bytes, manifest_json, changelog)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6)
		 RETURNING id, package_id, version, manifest_digest, blob_size_bytes,
		           manifest_json::text, changelog, yanked, created_at`,
		pkgID, p.Version, p.ManifestDigest, p.BlobSizeBytes, mJSONText, p.Changelog,
	).Scan(&rel.ID, &rel.PackageID, &rel.Version, &rel.ManifestDigest, &rel.BlobSizeBytes,
		&mText, &rel.Changelog, &rel.Yanked, &rel.CreatedAt); err != nil {
		return nil, fmt.Errorf("InsertRelease: insert release: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO skill_signatures (release_id, signature_b64, key_id, algorithm)
		 VALUES ($1, $2, $3, $4)`,
		rel.ID, p.SignatureB64, p.SignatureKeyID, p.SignatureAlgorithm,
	); err != nil {
		return nil, fmt.Errorf("InsertRelease: insert signature: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("InsertRelease: commit: %w", err)
	}
	rel.Slug = p.Slug
	rel.ManifestJSON = coerceJSON(mText)
	rel.SignatureB64 = p.SignatureB64
	rel.SignatureKeyID = p.SignatureKeyID
	rel.SignatureAlgorithm = p.SignatureAlgorithm
	return &rel, nil
}

// ---------------------------------------------------------------------------
// helpers.
// ---------------------------------------------------------------------------

func scanPublisher(row pgx.Row) (*PublisherRow, error) {
	var p PublisherRow
	if err := row.Scan(&p.ID, &p.Handle, &p.DisplayName, &p.PublicKeyPEM, &p.PublishTokenSHA256); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scanPublisher: %w", err)
	}
	return &p, nil
}

func scanReleases(rows pgx.Rows) ([]Release, error) {
	var out []Release
	for rows.Next() {
		var rel Release
		var mJSONText string
		if err := rows.Scan(
			&rel.ID, &rel.PackageID, &rel.Slug, &rel.Version, &rel.ManifestDigest,
			&rel.BlobSizeBytes, &mJSONText, &rel.Changelog, &rel.Yanked, &rel.CreatedAt,
			&rel.SignatureB64, &rel.SignatureKeyID, &rel.SignatureAlgorithm,
		); err != nil {
			return nil, fmt.Errorf("scanReleases: %w", err)
		}
		rel.ManifestJSON = coerceJSON(mJSONText)
		out = append(out, rel)
	}
	return out, rows.Err()
}

func coerceJSON(text string) map[string]any {
	if text == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return map[string]any{}
	}
	return m
}
