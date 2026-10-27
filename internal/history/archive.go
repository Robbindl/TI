package history

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Message struct {
	ChatJID        string
	RequestChatJID string
	MessageID      string
	SenderJID      string
	Timestamp      time.Time
	Chat           string
	Sender         string
	Text           string
	FromMe         bool
	MediaKind      string
	MediaName      string
	MediaMIME      string
	MediaData      []byte
}

type ReplayResult struct {
	Count        int
	Available    int
	ReachedStart bool
	Detail       string
}

type ReplayProgress func(string)

type Contact struct {
	JID          string
	Name         string
	MessageCount int
	LastContact  time.Time
}

type CallRecord struct {
	CallID     string
	CallerJID  string
	CallerName string
	Timestamp  time.Time
	Status     string
}

type CallContact struct {
	JID       string
	Name      string
	CallCount int
	LastCall  time.Time
	LastState string
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
		request_chat_jid TEXT NOT NULL DEFAULT '',
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
		{"request_chat_jid", `TEXT NOT NULL DEFAULT ''`},
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
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS call_history (
		call_id TEXT PRIMARY KEY,
		caller_jid TEXT NOT NULL,
		caller_name TEXT NOT NULL,
		timestamp_unix INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'offered'
	)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS call_history_date ON call_history(timestamp_unix)`); err != nil {
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
		(chat_jid, request_chat_jid, message_id, sender_jid, timestamp_unix, chat_name, sender_name, text, from_me, media_kind, media_name, media_mime, media_data)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid, message_id, sender_jid) DO UPDATE SET
		request_chat_jid=CASE WHEN excluded.request_chat_jid='' THEN message_history.request_chat_jid ELSE excluded.request_chat_jid END,
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
		if message.RequestChatJID == "" {
			message.RequestChatJID = message.ChatJID
		}
		if _, err := statement.ExecContext(ctx, message.ChatJID, message.RequestChatJID, message.MessageID, message.SenderJID, message.Timestamp.Unix(), message.Chat, message.Sender, message.Text, message.FromMe, message.MediaKind, message.MediaName, message.MediaMIME, nullableBytes(message.MediaData)); err != nil {
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

func (a *Archive) Range(ctx context.Context, chatJID string, from, until time.Time, after *Message, limit int) ([]Message, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("history page size must be positive")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	query := `SELECT message_id, sender_jid, timestamp_unix, chat_name, sender_name, text, from_me, media_kind, media_name, media_mime, media_data
		FROM message_history WHERE chat_jid = ? AND timestamp_unix >= ? AND timestamp_unix < ?
		ORDER BY timestamp_unix, message_id, sender_jid LIMIT ?`
	args := []any{chatJID, from.Unix(), until.Unix(), limit}
	if after != nil {
		query = `SELECT message_id, sender_jid, timestamp_unix, chat_name, sender_name, text, from_me, media_kind, media_name, media_mime, media_data
			FROM message_history WHERE chat_jid = ? AND timestamp_unix >= ? AND timestamp_unix < ?
			AND (timestamp_unix > ? OR (timestamp_unix = ? AND (message_id > ? OR (message_id = ? AND sender_jid > ?))))
			ORDER BY timestamp_unix, message_id, sender_jid LIMIT ?`
		args = []any{chatJID, from.Unix(), until.Unix(), after.Timestamp.Unix(), after.Timestamp.Unix(), after.MessageID, after.MessageID, after.SenderJID, limit}
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
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

func (a *Archive) CountRange(ctx context.Context, chatJID string, from, until time.Time) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var count int
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_history
		WHERE chat_jid = ? AND timestamp_unix >= ? AND timestamp_unix < ?`, chatJID, from.Unix(), until.Unix()).Scan(&count)
	return count, err
}

func (a *Archive) ContactsContacted(ctx context.Context, from, until time.Time) ([]Contact, error) {
	return a.contactsByDirection(ctx, from, until, false)
}

func (a *Archive) ContactsMessagedByMe(ctx context.Context, from, until time.Time) ([]Contact, error) {
	return a.contactsByDirection(ctx, from, until, true)
}

func (a *Archive) contactsByDirection(ctx context.Context, from, until time.Time, fromMe bool) ([]Contact, error) {
	if !until.After(from) {
		return nil, fmt.Errorf("contact report end must be later than its start")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	nameExpr := `COALESCE(
			NULLIF(CASE WHEN sender_name NOT IN ('', 'Unknown sender', 'Unknown chat', 'Unknown private chat') AND instr(sender_name, '@') = 0 THEN sender_name END, ''),
			NULLIF(CASE WHEN chat_name NOT IN ('', 'Unknown chat', 'Unknown private chat') AND instr(chat_name, '@') = 0 THEN chat_name END, ''),
			chat_jid
		)`
	if fromMe {
		nameExpr = `COALESCE(
			NULLIF(CASE WHEN chat_name NOT IN ('', 'Unknown sender', 'Unknown chat', 'Unknown private chat', 'You') AND instr(chat_name, '@') = 0 THEN chat_name END, ''),
			chat_jid
		)`
	}
	rows, err := a.db.QueryContext(ctx, `WITH ranked_contacts AS (
		SELECT chat_jid,
			`+nameExpr+` AS contact_name,
			timestamp_unix,
			COUNT(*) OVER (PARTITION BY chat_jid) AS message_count,
			ROW_NUMBER() OVER (PARTITION BY chat_jid ORDER BY timestamp_unix DESC, message_id DESC) AS row_number
		FROM message_history
		WHERE from_me = ? AND timestamp_unix >= ? AND timestamp_unix < ?
			AND (chat_jid GLOB '*@s.whatsapp.net' OR chat_jid GLOB '*@lid' OR chat_jid GLOB '*@c.us')
	)
	SELECT chat_jid, contact_name, message_count, timestamp_unix
	FROM ranked_contacts WHERE row_number = 1
	ORDER BY timestamp_unix DESC, contact_name COLLATE NOCASE`, fromMe, from.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var contacts []Contact
	for rows.Next() {
		var contact Contact
		var lastContact int64
		if err := rows.Scan(&contact.JID, &contact.Name, &contact.MessageCount, &lastContact); err != nil {
			return nil, err
		}
		contact.LastContact = time.Unix(lastContact, 0)
		contacts = append(contacts, contact)
	}
	return contacts, rows.Err()
}

func (a *Archive) SaveIncomingCall(ctx context.Context, call CallRecord) error {
	if call.CallID == "" || call.CallerJID == "" || call.Timestamp.IsZero() {
		return fmt.Errorf("call ID, caller, and timestamp are required")
	}
	if call.CallerName == "" {
		call.CallerName = call.CallerJID
	}
	if call.Status == "" {
		call.Status = "offered"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.db.ExecContext(ctx, `INSERT INTO call_history (call_id, caller_jid, caller_name, timestamp_unix, status)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(call_id) DO UPDATE SET
		caller_jid=excluded.caller_jid,
		caller_name=CASE WHEN excluded.caller_name='' THEN call_history.caller_name ELSE excluded.caller_name END,
		timestamp_unix=excluded.timestamp_unix,
		status=excluded.status`,
		call.CallID, call.CallerJID, call.CallerName, call.Timestamp.Unix(), call.Status)
	return err
}

func (a *Archive) UpdateCallStatus(ctx context.Context, callID, status string) error {
	if callID == "" || status == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err := a.db.ExecContext(ctx, `UPDATE call_history SET status = ? WHERE call_id = ?`, status, callID)
	return err
}

func (a *Archive) CallsContacted(ctx context.Context, from, until time.Time) ([]CallContact, error) {
	if !until.After(from) {
		return nil, fmt.Errorf("call report end must be later than its start")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.db.QueryContext(ctx, `WITH ranked_calls AS (
		SELECT caller_jid,
			COALESCE(
				NULLIF(CASE WHEN caller_name NOT IN ('', 'Unknown chat', 'Unknown private chat', 'Unknown sender') AND instr(caller_name, '@') = 0 THEN caller_name END, ''),
				caller_jid
			) AS caller_name,
			timestamp_unix,
			status,
			COUNT(*) OVER (PARTITION BY caller_jid) AS call_count,
			ROW_NUMBER() OVER (PARTITION BY caller_jid ORDER BY timestamp_unix DESC, call_id DESC) AS row_number
		FROM call_history
		WHERE timestamp_unix >= ? AND timestamp_unix < ?
			AND (caller_jid GLOB '*@s.whatsapp.net' OR caller_jid GLOB '*@lid' OR caller_jid GLOB '*@c.us')
	)
	SELECT caller_jid, caller_name, call_count, timestamp_unix, status
	FROM ranked_calls WHERE row_number = 1
	ORDER BY timestamp_unix DESC, caller_name COLLATE NOCASE`, from.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var callers []CallContact
	for rows.Next() {
		var caller CallContact
		var lastCall int64
		if err := rows.Scan(&caller.JID, &caller.Name, &caller.CallCount, &lastCall, &caller.LastState); err != nil {
			return nil, err
		}
		caller.LastCall = time.Unix(lastCall, 0)
		callers = append(callers, caller)
	}
	return callers, rows.Err()
}

func (a *Archive) Oldest(ctx context.Context, chatJID string) (Message, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var message Message
	var timestamp int64
	err := a.db.QueryRowContext(ctx, `SELECT request_chat_jid, message_id, sender_jid, timestamp_unix, chat_name, sender_name, text, from_me
		FROM message_history WHERE chat_jid = ? ORDER BY timestamp_unix, message_id LIMIT 1`, chatJID).
		Scan(&message.RequestChatJID, &message.MessageID, &message.SenderJID, &timestamp, &message.Chat, &message.Sender, &message.Text, &message.FromMe)
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
