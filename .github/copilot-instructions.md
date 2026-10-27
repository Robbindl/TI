# Project Notes

- This is a local Go WhatsApp-to-Telegram bridge.
- WhatsApp connectivity uses the unofficial whatsmeow library; preserve the README warnings and keep forwarding opt-in per chat.
- The Telegram bot is controlled only by the configured owner ID. Never log or commit bot tokens, WhatsApp session data, or message contents unnecessarily.
- Run `go test ./...` and `go vet ./...` after Go changes when the Go toolchain is available.
