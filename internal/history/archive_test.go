package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestArchiveCutoffAndDeduplication(t *testing.T) {
	archive, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()

	before := Message{ChatJID: "chat@s.whatsapp.net", MessageID: "old", SenderJID: "sender", Timestamp: time.Date(2026, 3, 14, 23, 0, 0, 0, time.UTC), Text: "before"}
	included := Message{
		ChatJID: "chat@s.whatsapp.net", MessageID: "new", SenderJID: "sender",
		Timestamp: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), Text: "included",
		MediaKind: "photo", MediaName: "photo.jpg", MediaMIME: "image/jpeg", MediaData: []byte("image-bytes"),
	}
	for _, message := range []Message{before, included, included} {
		if err := archive.Save(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}

	messages, err := archive.PendingSince(context.Background(), included.ChatJID, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Text != "included" || messages[0].MediaKind != "photo" || string(messages[0].MediaData) != "image-bytes" {
		t.Fatalf("expected one deduplicated message on/after cutoff; got %#v", messages)
	}
	if err := archive.MarkForwarded(context.Background(), messages[0]); err != nil {
		t.Fatal(err)
	}
	messages, err = archive.PendingSince(context.Background(), included.ChatJID, time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].MessageID != "old" {
		t.Fatalf("expected only the unforwarded older message; got %#v", messages)
	}
}

func TestOldestReturnsHistoryCursor(t *testing.T) {
	archive, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()

	first := Message{ChatJID: "chat@s.whatsapp.net", MessageID: "first", SenderJID: "sender", Timestamp: time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC), Text: "first"}
	second := Message{ChatJID: "chat@s.whatsapp.net", MessageID: "second", SenderJID: "sender", Timestamp: time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC), Text: "second"}
	if err := archive.SaveBatch(context.Background(), []Message{second, first}); err != nil {
		t.Fatal(err)
	}
	oldest, found, err := archive.Oldest(context.Background(), "chat@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if !found || oldest.MessageID != "first" {
		t.Fatalf("expected earliest message as cursor; got %#v, found=%v", oldest, found)
	}
}
