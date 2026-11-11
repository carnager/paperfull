package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Ref struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type Document struct {
	ID            int64     `json:"id"`
	Title         string    `json:"title"`
	Content       string    `json:"content,omitempty"`
	Created       string    `json:"created"`
	Added         time.Time `json:"added"`
	Modified      time.Time `json:"modified"`
	Correspondent *Ref      `json:"correspondent"`
	DocumentType  *Ref      `json:"document_type"`
	Tags          []Ref     `json:"tags"`
	Checksum      string    `json:"checksum"`
	OriginalName  string    `json:"original_name"`
	Mime          string    `json:"mime"`
	Path          string    `json:"path"`
	ArchivePath   string    `json:"archive_path,omitempty"`
	Pages         int       `json:"pages"`
	Inbox         bool      `json:"inbox"`
	Notes         string    `json:"notes"`
	// Suggestions for a sender or type that doesn't exist yet (from the AI
	// model). They become real once someone saves the document.
	SuggestedSender string `json:"suggested_sender,omitempty"`
	SuggestedType   string `json:"suggested_type,omitempty"`
	// Snippet is HTML (escaped text with <mark> around hits); only set by Search.
	Snippet string `json:"snippet,omitempty"`
}

func (d *Document) HasTag(id int64) bool {
	for _, t := range d.Tags {
		if t.ID == id {
			return true
		}
	}
	return false
}

func (d *Document) TagIDs() []int64 {
	ids := make([]int64, len(d.Tags))
	for i, t := range d.Tags {
		ids[i] = t.ID
	}
	return ids
}

const docCols = `d.id, d.title, d.created, d.added, d.modified, d.correspondent_id, IFNULL(c.name,''),
 d.document_type_id, IFNULL(t.name,''), d.checksum, d.original_name, d.mime, d.path, d.archive_path,
 d.pages, d.inbox, d.notes, d.suggested_sender, d.suggested_type`

const docJoins = ` FROM documents d
 LEFT JOIN correspondents c ON c.id = d.correspondent_id
 LEFT JOIN document_types t ON t.id = d.document_type_id`

type scanner interface{ Scan(dest ...any) error }

func scanDoc(sc scanner, extra ...any) (*Document, error) {
	var d Document
	var cid, tid sql.NullInt64
	var cname, tname, added, modified string
	var inbox int
	dest := []any{&d.ID, &d.Title, &d.Created, &added, &modified, &cid, &cname, &tid, &tname,
		&d.Checksum, &d.OriginalName, &d.Mime, &d.Path, &d.ArchivePath, &d.Pages, &inbox, &d.Notes, &d.SuggestedSender, &d.SuggestedType}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.Added, _ = time.Parse(time.RFC3339, added)
	d.Modified, _ = time.Parse(time.RFC3339, modified)
	if cid.Valid {
		d.Correspondent = &Ref{ID: cid.Int64, Name: cname}
	}
	if tid.Valid {
		d.DocumentType = &Ref{ID: tid.Int64, Name: tname}
	}
	d.Inbox = inbox != 0
	d.Tags = []Ref{}
	return &d, nil
}

func (s *Store) loadTags(ctx context.Context, docs []*Document) error {
	if len(docs) == 0 {
		return nil
	}
	byID := make(map[int64]*Document, len(docs))
	ph := make([]string, 0, len(docs))
	args := make([]any, 0, len(docs))
	for _, d := range docs {
		byID[d.ID] = d
		ph = append(ph, "?")
		args = append(args, d.ID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT dt.document_id, g.id, g.name, g.color FROM document_tags dt
	  JOIN tags g ON g.id = dt.tag_id WHERE dt.document_id IN (`+strings.Join(ph, ",")+`)
	  ORDER BY g.name COLLATE NOCASE`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var did int64
		var r Ref
		if err := rows.Scan(&did, &r.ID, &r.Name, &r.Color); err != nil {
			return err
		}
		if d := byID[did]; d != nil {
			d.Tags = append(d.Tags, r)
		}
	}
	return rows.Err()
}

func (s *Store) GetDocument(ctx context.Context, id int64) (*Document, error) {
	var content string
	d, err := scanDoc(s.db.QueryRowContext(ctx, `SELECT `+docCols+`, d.content`+docJoins+` WHERE d.id = ?`, id), &content)
	if err != nil {
		return nil, err
	}
	d.Content = content
	return d, s.loadTags(ctx, []*Document{d})
}

func (s *Store) DocumentByChecksum(ctx context.Context, sum string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM documents WHERE checksum = ?`, sum).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

type NewDocument struct {
	Title, Content, Created, Checksum, OriginalName, Mime string
	CorrespondentID, DocumentTypeID                       *int64
	TagIDs                                                []int64
	Pages                                                 int
	Inbox                                                 bool
	SuggestedSender, SuggestedType                        string
}

func (s *Store) InsertDocument(ctx context.Context, n NewDocument) (int64, error) {
	now := nowStr()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO documents(title, content, created, added, modified,
	  correspondent_id, document_type_id, checksum, original_name, mime, pages, inbox, suggested_sender, suggested_type)
	  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.Title, n.Content, n.Created, now, now, nullable(n.CorrespondentID), nullable(n.DocumentTypeID),
		n.Checksum, n.OriginalName, n.Mime, n.Pages, boolInt(n.Inbox), n.SuggestedSender, n.SuggestedType)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, t := range n.TagIDs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?,?)`, id, t); err != nil {
			return 0, err
		}
	}
	if err := reindexIDs(ctx, tx, []int64{id}); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) SetPaths(ctx context.Context, id int64, path, archive string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE documents SET path=?, archive_path=? WHERE id=?`, path, archive, id)
	return err
}

// SetContent replaces OCR results after reprocessing.
func (s *Store) SetContent(ctx context.Context, id int64, content string, pages int, archive string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET content=?, pages=?, archive_path=?, modified=? WHERE id=?`,
		content, pages, archive, nowStr(), id); err != nil {
		return err
	}
	if err := reindexIDs(ctx, tx, []int64{id}); err != nil {
		return err
	}
	return tx.Commit()
}

// NullID is an optional foreign key: Valid=false means "unset it".
type NullID struct {
	ID    int64
	Valid bool
}

// DocUpdate holds the fields to change; nil fields stay as they are.
type DocUpdate struct {
	Title, Created, Notes       *string
	Inbox                       *bool
	Correspondent, DocumentType *NullID
	Tags                        *[]int64 // replaces all tags
	AddTags, RemoveTags         []int64
}

func (u DocUpdate) Empty() bool {
	return u.Title == nil && u.Created == nil && u.Notes == nil && u.Inbox == nil &&
		u.Correspondent == nil && u.DocumentType == nil && u.Tags == nil &&
		len(u.AddTags) == 0 && len(u.RemoveTags) == 0
}

func ValidDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func (s *Store) UpdateDocuments(ctx context.Context, ids []int64, u DocUpdate) error {
	if u.Created != nil && !ValidDate(*u.Created) {
		return fmt.Errorf("created must be YYYY-MM-DD, got %q", *u.Created)
	}
	if u.Title != nil && strings.TrimSpace(*u.Title) == "" {
		return errors.New("title must not be empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var sets []string
	var args []any
	if u.Title != nil {
		sets, args = append(sets, "title=?"), append(args, strings.TrimSpace(*u.Title))
	}
	if u.Created != nil {
		sets, args = append(sets, "created=?"), append(args, *u.Created)
	}
	if u.Notes != nil {
		sets, args = append(sets, "notes=?"), append(args, *u.Notes)
	}
	if u.Inbox != nil {
		sets, args = append(sets, "inbox=?"), append(args, boolInt(*u.Inbox))
	}
	if u.Correspondent != nil {
		var v any
		if u.Correspondent.Valid {
			v = u.Correspondent.ID
		}
		sets, args = append(sets, "correspondent_id=?", "suggested_sender=''"), append(args, v)
	}
	if u.DocumentType != nil {
		var v any
		if u.DocumentType.Valid {
			v = u.DocumentType.ID
		}
		sets, args = append(sets, "document_type_id=?", "suggested_type=''"), append(args, v)
	}
	sets, args = append(sets, "modified=?"), append(args, nowStr())

	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE documents SET `+strings.Join(sets, ", ")+` WHERE id=?`, append(args, id)...)
		if err != nil {
			if strings.Contains(err.Error(), "FOREIGN KEY") {
				return errors.New("unknown correspondent or document type")
			}
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("document %d: %w", id, ErrNotFound)
		}
		if u.Tags != nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM document_tags WHERE document_id=?`, id); err != nil {
				return err
			}
		}
		add := u.AddTags
		if u.Tags != nil {
			add = append(append([]int64{}, *u.Tags...), add...)
		}
		for _, t := range add {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO document_tags(document_id, tag_id) VALUES (?,?)`, id, t); err != nil {
				if strings.Contains(err.Error(), "FOREIGN KEY") {
					return fmt.Errorf("unknown tag %d", t)
				}
				return err
			}
		}
		for _, t := range u.RemoveTags {
			if _, err := tx.ExecContext(ctx, `DELETE FROM document_tags WHERE document_id=? AND tag_id=?`, id, t); err != nil {
				return err
			}
		}
	}
	if err := reindexIDs(ctx, tx, ids); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDocument removes the row and returns what it was, so files can be cleaned up.
func (s *Store) DeleteDocument(ctx context.Context, id int64) (*Document, error) {
	d, err := s.GetDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id=?`, id); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid=?`, id); err != nil {
		return nil, err
	}
	return d, tx.Commit()
}

// NextInbox returns the oldest inbox document other than the given one.
func (s *Store) NextInbox(ctx context.Context, exclude int64) (int64, bool) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM documents WHERE inbox=1 AND id != ? ORDER BY added, id LIMIT 1`, exclude).Scan(&id)
	return id, err == nil
}

func (s *Store) AllIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM documents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TrainDoc is what the classifier learns from: a reviewed (non-inbox) document.
type TrainDoc struct {
	Content         string
	CorrespondentID int64
	DocumentTypeID  int64
	TagIDs          []int64
}

// EachTrainingDoc calls fn for every document that has been reviewed.
func (s *Store) EachTrainingDoc(ctx context.Context, fn func(TrainDoc)) error {
	tags := map[int64][]int64{}
	rows, err := s.db.QueryContext(ctx, `SELECT dt.document_id, dt.tag_id FROM document_tags dt
	  JOIN documents d ON d.id = dt.document_id WHERE d.inbox = 0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d, t int64
		if err := rows.Scan(&d, &t); err != nil {
			rows.Close()
			return err
		}
		tags[d] = append(tags[d], t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id, content, IFNULL(correspondent_id,0), IFNULL(document_type_id,0)
	  FROM documents WHERE inbox = 0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var td TrainDoc
		if err := rows.Scan(&id, &td.Content, &td.CorrespondentID, &td.DocumentTypeID); err != nil {
			return err
		}
		td.TagIDs = tags[id]
		fn(td)
	}
	return rows.Err()
}

type YearCount struct {
	Year  string
	Count int
}

// Years lists the years documents are dated in, newest first.
func (s *Store) Years(ctx context.Context) ([]YearCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT substr(created,1,4) y, COUNT(*) FROM documents GROUP BY y ORDER BY y DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []YearCount
	for rows.Next() {
		var y YearCount
		if err := rows.Scan(&y.Year, &y.Count); err != nil {
			return nil, err
		}
		out = append(out, y)
	}
	return out, rows.Err()
}

// SetSuggestions stores AI suggestions for a document's sender and type.
func (s *Store) SetSuggestions(ctx context.Context, id int64, sender, typ string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE documents SET suggested_sender=?, suggested_type=? WHERE id=?`, sender, typ, id)
	return err
}
