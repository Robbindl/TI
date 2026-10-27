package whatsapp

import (
	"testing"
	"time"

	"github.com/local/wa-telegram-bridge/internal/history"
)

func TestIsOlderCursor(t *testing.T) {
	current := history.Message{MessageID: "middle", Timestamp: time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local)}
	if !isOlderCursor(history.Message{MessageID: "earlier", Timestamp: current.Timestamp.Add(-time.Second)}, current) {
		t.Fatal("an earlier timestamp should move the history cursor back")
	}
	if !isOlderCursor(history.Message{MessageID: "different", Timestamp: current.Timestamp}, current) {
		t.Fatal("a different message at the same second should allow paging")
	}
	if isOlderCursor(current, current) {
		t.Fatal("the same message should not advance the history cursor")
	}
}
