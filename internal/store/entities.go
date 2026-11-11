package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Kind is one of the three kinds of things a document can be sorted by.
type Kind string

const (
	KindTag           Kind = "tags"
	KindCorrespondent Kind = "correspondents"
	KindDocumentType  Kind = "document_types"
)

func (k Kind) Valid() bool {
	return k == KindTag || k == KindCorrespondent || k == KindDocumentType
}

func (k Kind) Singular() string {
	switch k {
	case KindTag:
		return "tag"
	case KindCorrespondent:
		return "correspondent"
	default:
		return "document type"
	}
}

// Matching algorithms, same idea as paperless.
var Algorithms = []string{"auto", "any", "all", "literal", "regex", "none"}

func ValidAlgorithm(a string) bool {
	for _, x := range Algorithms {
		if x == a {
			return true
		}
	}
	return false
}

type Entity struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Match         string `json:"match"`
	Algorithm     string `json:"algorithm"`
	CaseSensitive bool   `json:"case_sensitive"`
	Color         string `json:"color,omitempty"`
	DocCount      int    `json:"document_count"`
}

func countExpr(k Kind) string {
	switch k {
	case KindTag:
		return `(SELECT COUNT(*) FROM document_tags WHERE tag_id = e.id)`
	case KindCorrespondent:
		return `(SELECT COUNT(*) FROM documents WHERE correspondent_id = e.id)`
	default:
		return `(SELECT COUNT(*) FROM documents WHERE document_type_id = e.id)`
	}
}

func (s *Store) ListEntities(ctx context.Context, k Kind) ([]Entity, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("invalid kind %q", k)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.name, e.match, e.algorithm, e.case_sensitive, e.color, `+
		countExpr(k)+` FROM `+string(k)+` e ORDER BY e.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entity
	for rows.Next() {
		var e Entity
		var cs int
		if err := rows.Scan(&e.ID, &e.Name, &e.Match, &e.Algorithm, &cs, &e.Color, &e.DocCount); err != nil {
			return nil, err
		}
		e.CaseSensitive = cs != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetEntity(ctx context.Context, k Kind, id int64) (*Entity, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("invalid kind %q", k)
	}
	var e Entity
	var cs int
	err := s.db.QueryRowContext(ctx, `SELECT e.id, e.name, e.match, e.algorithm, e.case_sensitive, e.color, `+
		countExpr(k)+` FROM `+string(k)+` e WHERE e.id = ?`, id).
		Scan(&e.ID, &e.Name, &e.Match, &e.Algorithm, &cs, &e.Color, &e.DocCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	e.CaseSensitive = cs != 0
	return &e, err
}

func (s *Store) EntityByName(ctx context.Context, k Kind, name string) (*Entity, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("invalid kind %q", k)
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM `+string(k)+` WHERE name = ?`, strings.TrimSpace(name)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetEntity(ctx, k, id)
}

// EnsureEntity returns the id of the entity with that name, creating it if needed.
func (s *Store) EnsureEntity(ctx context.Context, k Kind, name string) (int64, error) {
	e, err := s.EntityByName(ctx, k, name)
	if err == nil {
		return e.ID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	return s.CreateEntity(ctx, k, Entity{Name: name, Algorithm: "auto"})
}

func validate(e *Entity) error {
	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" {
		return errors.New("name must not be empty")
	}
	if e.Algorithm == "" {
		e.Algorithm = "auto"
	}
	if !ValidAlgorithm(e.Algorithm) {
		return fmt.Errorf("unknown algorithm %q (use one of %s)", e.Algorithm, strings.Join(Algorithms, ", "))
	}
	if e.Algorithm == "regex" {
		if _, err := compileRegex(e.Match, e.CaseSensitive); err != nil {
			return fmt.Errorf("invalid regex: %w", err)
		}
	}
	return nil
}

func (s *Store) CreateEntity(ctx context.Context, k Kind, e Entity) (int64, error) {
	if !k.Valid() {
		return 0, fmt.Errorf("invalid kind %q", k)
	}
	if err := validate(&e); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO `+string(k)+`(name, match, algorithm, case_sensitive, color) VALUES (?,?,?,?,?)`,
		e.Name, e.Match, e.Algorithm, boolInt(e.CaseSensitive), e.Color)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, fmt.Errorf("a %s named %q already exists", k.Singular(), e.Name)
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateEntity(ctx context.Context, k Kind, e Entity) error {
	if !k.Valid() {
		return fmt.Errorf("invalid kind %q", k)
	}
	if err := validate(&e); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE `+string(k)+` SET name=?, match=?, algorithm=?, case_sensitive=?, color=? WHERE id=?`,
		e.Name, e.Match, e.Algorithm, boolInt(e.CaseSensitive), e.Color, e.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("a %s named %q already exists", k.Singular(), e.Name)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	ids, err := docsReferencing(ctx, tx, k, e.ID)
	if err != nil {
		return err
	}
	if err := reindexIDs(ctx, tx, ids); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteEntity removes the entity and returns the ids of documents that referenced it.
func (s *Store) DeleteEntity(ctx context.Context, k Kind, id int64) ([]int64, error) {
	if !k.Valid() {
		return nil, fmt.Errorf("invalid kind %q", k)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids, err := docsReferencing(ctx, tx, k, id)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM `+string(k)+` WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if err := reindexIDs(ctx, tx, ids); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

func docsReferencing(ctx context.Context, tx *sql.Tx, k Kind, id int64) ([]int64, error) {
	var q string
	switch k {
	case KindTag:
		q = `SELECT document_id FROM document_tags WHERE tag_id = ?`
	case KindCorrespondent:
		q = `SELECT id FROM documents WHERE correspondent_id = ?`
	default:
		q = `SELECT id FROM documents WHERE document_type_id = ?`
	}
	rows, err := tx.QueryContext(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		ids = append(ids, x)
	}
	return ids, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
