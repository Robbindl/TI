# WhatsApp to Telegram Bridge

A local Go service that links one WhatsApp account with a terminal QR code and forwards messages from chats you explicitly enable to your own Telegram bot. Chat selection is managed in the Telegram private chat with the bot using `/chats` and inline buttons.

## Important

WhatsApp connectivity is provided by the unofficial `whatsmeow` library, not the WhatsApp Business Platform. It may violate WhatsApp's terms, break when WhatsApp changes its protocol, or lead to the linked account being restricted. Use only an account and conversations you are authorized to access, and consider the privacy of everyone whose messages may be forwarded. The bridge does not forward view-once messages. It forwards text and supported media in both directions of the conversation (messages you send and messages you receive) to Telegram, but it does not send Telegram messages back to WhatsApp.

The WhatsApp linked-device session is stored in `data/whatsapp.db`. Synced text and captions are archived locally in `data/message-history.db` so date-based replay can work after WhatsApp's sync has completed. Treat both databases as sensitive. The Telegram bot token and owner ID are read from environment variables, not files in the repository.

## Requirements

- Go 1.25 or newer
- A Telegram bot token from BotFather
- Your numeric Telegram user ID
- Internet access while the bridge is running

The project uses the pure-Go SQLite driver, so a separate C compiler is not needed.

## Run on Windows

From PowerShell in the project folder:

```powershell
$env:TELEGRAM_BOT_TOKEN = "your BotFather token"
$env:TELEGRAM_OWNER_ID = "your numeric Telegram user ID"
go mod tidy
go run ./cmd/bridge
```

Keep the environment variables in the same PowerShell session while the program runs. Do not send your bot token in chat or commit it to source control.

On first start, scan the QR code printed in the terminal from WhatsApp's **Linked devices** screen. After the connection completes, the terminal prints contacts and groups available to the bridge. Open your Telegram bot, send `/start`, then `/chats`. For each chat, **ON/OFF** controls forwarding, and **Since date** prompts for a `YYYY-MM-DD` cutoff. Setting a cutoff enables that chat, replays available archived messages on or after the date, and forwards new messages from that date onward. Selection and dates persist across restarts. The bot only accepts commands and callbacks from the configured owner ID in the bot's private chat.

History replay first uses the local archive, then requests older history from the primary phone in batches of 50 using the linked-device on-demand history feature. It stops when the requested date is reached, the phone returns no earlier messages, no message anchor exists, or 200 batches have been requested. WhatsApp and the primary phone decide what history is available; this cannot recover deleted messages, history the phone withholds, or an arbitrary date range the phone refuses. The archive deduplicates messages and stores supported text/caption content and media locally. The cutoff remains active for new live messages even if backfill is unavailable.

To stop the bridge, press `Ctrl+C`. The WhatsApp pairing is retained in the local database, so later runs normally reconnect without another QR scan.

## Checks

```powershell
go test ./...
go vet ./...
go build -o wa-telegram-bridge.exe ./cmd/bridge
```

## Current scope

- One linked WhatsApp account and one Telegram owner
- Contacts and joined groups shown in the Telegram selection menu
- Explicit per-chat forwarding allowlist and optional date cutoff, stored in `data/selected-chats.json`
- Incoming and outgoing WhatsApp text/captions forwarded to the owner's Telegram private chat
- Images, videos, voice notes, audio, stickers, and documents uploaded to Telegram when within the Bot API upload limit
- WhatsApp call offers, group-call notices, acceptances, and terminations sent as notifications for enabled chats
- Synced text/caption history stored in `data/message-history.db` and replayed from the selected date
- Telegram-to-WhatsApp message sending is not implemented
- View-once content is ignored
