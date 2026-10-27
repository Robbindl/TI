package history

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Message struct {
	ChatJID   string
	MessageID string
	SenderJID string
	Timestamp time.Time
	Chat      string
	Sender    string
	Text      string
	FromMe    bool
	MediaKind string
	MediaName string
	MediaMIME string
	MediaData []byte
}

type Archive struct {
	mu sync.Mutex
	db *sql.DB
}

func Open(path string) (*Archive, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS message_history (
		chat_jid TEXT NOT NULL,
		message_id TEXT NOT NULL,
		sender_jid TEXT NOT NULL,
		timestamp_unix INTEGER NOT NULL,
		chat_name TEXT NOT NULL,
		sender_name TEXT NOT NULL,
		text TEXT NOT NULL,
		from_me INTEGER NOT NULL,
		media_kind TEXT NOT NULL DEFAULT '',
		media_name TEXT NOT NULL DEFAULT '',
		media_mime TEXT NOT NULL DEFAULT '',
		media_data BLOB,
		forwarded INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (chat_jid, message_id, sender_jid)
	)`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, column := range []struct{ name, declaration string }{
		{"media_kind", `TEXT NOT NULL DEFAULT ''`},
		{"media_name", `TEXT NOT NULL DEFAULT ''`},
		{"media_mime", `TEXT NOT NULL DEFAULT ''`},
		{"media_data", `BLOB`},
	} {
		if err := ensureColumn(db, column.name, column.declaration); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS message_history_date ON message_history(chat_jid, timestamp_unix)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Archive{db: db}, nil
}

func ensureColumn(db *sql.DB, name, declaration string) error {
	rows, err := db.Query(`PRAGMA table_info(message_history)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var column, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if column == name {
			found = true
		}
	}
	rowsErr := rows.Err()
	_ = rows.Close()
	if rowsErr != nil {
		return rowsErr
	}
	if found {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE message_history ADD COLUMN ` + name + ` ` + declaration)
	return err
}

func (a *Archive) Close() error {
	return a.db.Close()
}

func (a *Archive) Save(ctx context.Context, message Message) error {
	return a.SaveBatch(ctx, []Message{message})
}

func (a *Archive) SaveBatch(ctx context.Context, messages []Message) error {
	if len(messages) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO message_history
		(chat_jid, message_id, sender_jid, timestamp_unix, chat_name, sender_name, text, from_me, media_kind, media_name, media_mime, media_data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid, message_id, sender_jid) DO UPDATE SET
		media_kind=CASE WHEN excluded.media_kind='' THEN message_history.media_kind ELSE excluded.media_kind END,
		media_name=CASE WHEN excluded.media_name='' THEN message_history.media_name ELSE excluded.media_name END,
		media_mime=CASE WHEN excluded.media_mime='' THEN message_history.media_mime ELSE excluded.media_mime END,
		media_data=COALESCE(excluded.media_data, message_history.media_data)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer statement.Close()
	for _, message := range messages {
		if message.MessageID == "" || message.ChatJID == "" || message.Text == "" {
			continue
		}
		if _, err := statement.ExecContext(ctx, message.ChatJID, message.MessageID, message.SenderJID, message.Timestamp.Unix(), message.Chat, message.Sender, message.Text, message.FromMe, message.MediaKind, message.MediaName, message.MediaMIME, nullableBytes(message.MediaData)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (a *Archive) PendingSince(ctx context.Context, chatJID string, since time.Time, limit int) ([]Message, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.db.QueryContext(ctx, `SELECT message_id, sender_jid, timestamp_unix, chat_name, sender_name, text, from_me, media_kind, media_name, media_mime, media_data
		FROM message_history WHERE chat_jid = ? AND timestamp_unix >= ? AND forwarded = 0
		ORDER BY timestamp_unix, message_id LIMIT ?`, chatJID, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []Message
	for rows.Next() {
		var message Message
		var timestamp int64
		if err := rows.Scan(&message.MessageID, &message.SenderJID, &timestamp, &message.Chat, &message.Sender, &message.Text, &message.FromMe, &message.MediaKind, &message.MediaName, &message.MediaMIME, &message.MediaData); err != nil {
			return nil, err
		}
		message.ChatJID = chatJID
		message.Timestamp = time.Unix(timestamp, 0)
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (a *Archive) Oldest(ctx context.Context, chatJID string) (Message, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var message Message
	var timestamp int64
	err := a.db.QueryRowContext(ctx, `SELECT message_id, sender_jid, timestamp_unix, chat_name, sender_name, text, from_me
		FROM message_history WHERE chat_jid = ? ORDER BY timestamp_unix, message_id LIMIT 1`, chatJID).
		Scan(&message.MessageID, &message.SenderJID, &timestamp, &message.Chat, &message.Sender, &message.Text, &message.FromMe)
	if err == sql.ErrNoRows {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, err
	}
	message.ChatJID = chatJID
	message.Timestamp = time.Unix(timestamp, 0)
	return message, true, nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func (a *Archive) MarkForwarded(ctx context.Context, message Message) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.db.ExecContext(ctx, `UPDATE message_history SET forwarded = 1
		WHERE chat_jid = ? AND message_id = ? AND sender_jid = ?`, message.ChatJID, message.MessageID, message.SenderJID)
	return err
}
