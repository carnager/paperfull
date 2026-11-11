// Package store keeps documents, their metadata and the full-text index in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS correspondents (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  match TEXT NOT NULL DEFAULT '',
  algorithm TEXT NOT NULL DEFAULT 'auto',
  case_sensitive INTEGER NOT NULL DEFAULT 0,
  color TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS document_types (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  match TEXT NOT NULL DEFAULT '',
  algorithm TEXT NOT NULL DEFAULT 'auto',
  case_sensitive INTEGER NOT NULL DEFAULT 0,
  color TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS tags (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE COLLATE NOCASE,
  match TEXT NOT NULL DEFAULT '',
  algorithm TEXT NOT NULL DEFAULT 'auto',
  case_sensitive INTEGER NOT NULL DEFAULT 0,
  color TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS documents (
  id INTEGER PRIMARY KEY,
  title TEXT NOT NULL,
  content TEXT NOT NULL DEFAULT '',
  created TEXT NOT NULL,
  added TEXT NOT NULL,
  modified TEXT NOT NULL,
  correspondent_id INTEGER REFERENCES correspondents(id) ON DELETE SET NULL,
  document_type_id INTEGER REFERENCES document_types(id) ON DELETE SET NULL,
  checksum TEXT NOT NULL UNIQUE,
  original_name TEXT NOT NULL DEFAULT '',
  mime TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  archive_path TEXT NOT NULL DEFAULT '',
  pages INTEGER NOT NULL DEFAULT 0,
  inbox INTEGER NOT NULL DEFAULT 1,
  notes TEXT NOT NULL DEFAULT '',
  suggested_sender TEXT NOT NULL DEFAULT '',
  suggested_type TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS documents_created ON documents(created);
CREATE INDEX IF NOT EXISTS documents_corr ON documents(correspondent_id);
CREATE INDEX IF NOT EXISTS documents_type ON documents(document_type_id);
CREATE INDEX IF NOT EXISTS documents_inbox ON documents(inbox);
CREATE TABLE IF NOT EXISTS document_tags (
  document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  tag_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (document_id, tag_id)
);
CREATE INDEX IF NOT EXISTS document_tags_tag ON document_tags(tag_id);
CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(
  title, content, meta,
  tokenize = 'trigram remove_diacritics 1'
);
`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)" +
		"&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Stats struct {
	Documents      int `json:"documents"`
	Inbox          int `json:"inbox"`
	Tags           int `json:"tags"`
	Correspondents int `json:"correspondents"`
	DocumentTypes  int `json:"document_types"`
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `SELECT
	  (SELECT COUNT(*) FROM documents),
	  (SELECT COUNT(*) FROM documents WHERE inbox=1),
	  (SELECT COUNT(*) FROM tags),
	  (SELECT COUNT(*) FROM correspondents),
	  (SELECT COUNT(*) FROM document_types)`).
		Scan(&st.Documents, &st.Inbox, &st.Tags, &st.Correspondents, &st.DocumentTypes)
	return st, err
}

func nowStr() string { return time.Now().UTC().Format(time.RFC3339) }

func nullable(p *int64) any {
	if p == nil || *p == 0 {
		return nil
	}
	return *p
}

// metaExpr builds the searchable "meta" column: names of correspondent, type
// and tags plus notes and the original file name.
const reindexSelect = `
INSERT INTO docs_fts(rowid, title, content, meta)
SELECT d.id, d.title, d.content,
  IFNULL(c.name,'') || ' ' || IFNULL(t.name,'') || ' ' ||
  IFNULL((SELECT group_concat(g.name, ' ') FROM document_tags dt JOIN tags g ON g.id = dt.tag_id WHERE dt.document_id = d.id), '') ||
  ' ' || d.notes || ' ' || d.original_name
FROM documents d
LEFT JOIN correspondents c ON c.id = d.correspondent_id
LEFT JOIN document_types t ON t.id = d.document_type_id
WHERE `

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func reindexIDs(ctx context.Context, ex execer, ids []int64) error {
	for _, id := range ids {
		if _, err := ex.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid = ?`, id); err != nil {
			return err
		}
		if _, err := ex.ExecContext(ctx, reindexSelect+`d.id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// Reindex rebuilds the whole full-text index.
func (s *Store) Reindex(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM docs_fts`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, reindexSelect+`1`); err != nil {
		return err
	}
	return tx.Commit()
}

// migrate adds columns that newer versions expect to older databases.
func migrate(db *sql.DB) error {
	have := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('documents')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	for _, col := range []string{"suggested_sender", "suggested_type"} {
		if !have[col] {
			if _, err := db.Exec(`ALTER TABLE documents ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	return nil
}
