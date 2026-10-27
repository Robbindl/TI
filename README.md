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

On first start, scan the QR code printed in the terminal from WhatsApp's **Linked devices** screen. After the connection completes, the terminal prints contacts and groups available to the bridge. Open your Telegram bot, send `/start`, then `/chats`. For each chat, **ON/OFF** controls live forwarding, **Since** sets its live-forwarding cutoff, and **History range** asks for inclusive start and end dates and sends that range again on every request. The bot searches its local archive and requests older pages from WhatsApp as needed. The bot only accepts commands and callbacks from the configured owner ID in the bot's private chat.

History replay first uses the local archive, then requests older history from the primary phone in batches of 50 using the linked-device on-demand history feature. It continues until the selected start date is reached or WhatsApp sends no older page. The bot reports how many messages it sent, how many were archived for the range, and whether the requested start date was reached. The local archive lets you request the same range again without filtering out messages sent previously. WhatsApp and the primary phone decide what history is available; the bridge cannot retrieve deleted messages or history WhatsApp does not provide to linked devices. Keep WhatsApp open on the primary phone while requesting older pages. The archive stores supported text/caption content and media locally.

To stop the bridge, press `Ctrl+C`. The WhatsApp pairing is retained in the local database, so later runs normally reconnect without another QR scan.

## Checks

```powershell
go test ./...
go vet ./...
go build -o wa-telegram-bridge.exe ./cmd/bridge
```

## Deploy to Fly.io

This is a single background worker, not an HTTP service. Run one Fly Machine and attach a persistent volume at `/app/data`; the WhatsApp session, selected chats, and message history are stored there.

Install and authenticate with `flyctl`, then run these commands from the project folder:

```powershell
fly auth login
fly apps create ti-wa-telegram-bridge
fly volumes create bridge_data --app ti-wa-telegram-bridge --region lax --size 1
fly secrets set TELEGRAM_BOT_TOKEN="your BotFather token" TELEGRAM_OWNER_ID="your numeric Telegram user ID" --app ti-wa-telegram-bridge
fly deploy --app ti-wa-telegram-bridge
```

If the app name is already taken, change `app` in `fly.toml` and use the same name in these commands. Watch the first-start QR code with:

```powershell
fly logs --app ti-wa-telegram-bridge
```

Then scan it from WhatsApp's **Linked devices** screen. Keep the app at one Machine because the Fly volume is local to that Machine and is not shared automatically.

## Run as a Windows service

For a local always-on setup, open PowerShell as Administrator in the project folder and pair WhatsApp once interactively:

```powershell
.\install-windows-service.ps1 -Action pair
```

Scan the QR code, then press `Ctrl+C` after the WhatsApp connection succeeds. Register the automatic service afterward:

```powershell
.\install-windows-service.ps1 -Action install
```

The installer prompts for the Telegram bot token and owner ID, builds the executable, registers an automatic Windows service, and configures restart-on-failure. Service logs are written to `data/service.log` and the WhatsApp session remains in `data/`.

Manage the service with:

```powershell
.\install-windows-service.ps1 -Action status
.\install-windows-service.ps1 -Action stop
.\install-windows-service.ps1 -Action start
.\install-windows-service.ps1 -Action uninstall
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
