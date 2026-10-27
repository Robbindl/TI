package selection

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTogglePersistsSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "selected-chats.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	enabled, err := store.Toggle("123@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || !store.Enabled("123@s.whatsapp.net") {
		t.Fatal("expected chat to be enabled")
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Enabled("123@s.whatsapp.net") {
		t.Fatal("expected selection to survive reopening")
	}

	enabled, err = reopened.Toggle("123@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	if enabled || reopened.Enabled("123@s.whatsapp.net") {
		t.Fatal("expected chat to be disabled")
	}
}

func TestUnselectedChatIsDisabled(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Enabled("unselected@s.whatsapp.net") {
		t.Fatal("unselected chats must not forward")
	}
}

func TestCutoffAndLegacySelectionPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selection.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled("chat@s.whatsapp.net", true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSince("chat@s.whatsapp.net", "2026-03-15"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Enabled("chat@s.whatsapp.net") || reopened.Since("chat@s.whatsapp.net") != "2026-03-15" {
		t.Fatal("expected enabled state and cutoff to persist")
	}

	legacyPath := filepath.Join(t.TempDir(), "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(`{"legacy@s.whatsapp.net":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := Open(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.Enabled("legacy@s.whatsapp.net") {
		t.Fatal("expected legacy boolean selection to be preserved")
	}
}
