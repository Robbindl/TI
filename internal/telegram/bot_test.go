package telegram

import "testing"

func TestMediaEndpoint(t *testing.T) {
	cases := []struct {
		kind, method, field string
	}{
		{"photo", "sendPhoto", "photo"},
		{"sticker", "sendSticker", "sticker"},
		{"voice", "sendVoice", "voice"},
		{"audio", "sendAudio", "audio"},
		{"video", "sendVideo", "video"},
		{"animation", "sendAnimation", "animation"},
		{"document", "sendDocument", "document"},
	}
	for _, test := range cases {
		method, field := mediaEndpoint(test.kind)
		if method != test.method || field != test.field {
			t.Errorf("mediaEndpoint(%q) = (%q, %q), want (%q, %q)", test.kind, method, field, test.method, test.field)
		}
	}
	if method, field := mediaEndpoint("unknown"); method != "" || field != "" {
		t.Errorf("unexpected endpoint for unknown media: (%q, %q)", method, field)
	}
}
