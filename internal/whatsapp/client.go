package whatsapp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/local/wa-telegram-bridge/internal/history"
	"github.com/local/wa-telegram-bridge/internal/selection"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite"
	"rsc.io/qr"
)

type Chat struct {
	JID  string
	Name string
}

type Message struct {
	ChatJID    string
	MessageID  string
	SenderJID  string
	Timestamp  time.Time
	Chat       string
	Sender     string
	Text       string
	FromMe     bool
	MediaKind  string
	MediaName  string
	MediaMIME  string
	MediaData  []byte
	IsViewOnce bool
}

type Client struct {
	client  *whatsmeow.Client
	selects *selection.Store
	archive *history.Archive

	historySyncEvents chan struct{}
	historyRequestMu  sync.Mutex

	mu    sync.RWMutex
	chats []Chat
}

func New(selects *selection.Store) *Client {
	return &Client{selects: selects, historySyncEvents: make(chan struct{}, 8)}
}

func (c *Client) Run(ctx context.Context, onMessage func(Message) error, onConnected func()) error {
	defer func() {
		files, _ := filepath.Glob(filepath.Join("data", "whatsapp-link-qr-*.png"))
		for _, file := range files {
			_ = os.Remove(file)
		}
	}()
	logger := waLog.Stdout("WhatsApp", "INFO", true)
	container, err := sqlstore.New(ctx, "sqlite", "file:data/whatsapp.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)", logger)
	if err != nil {
		return fmt.Errorf("open WhatsApp session database: %w", err)
	}
	defer container.Close()
	archive, err := history.Open(filepath.Join("data", "message-history.db"))
	if err != nil {
		return fmt.Errorf("open local message history: %w", err)
	}
	c.archive = archive
	defer archive.Close()

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("load WhatsApp device: %w", err)
	}
	if device.ID != nil && os.Getenv("WHATSAPP_FORCE_RELINK") == "1" {
		if err := device.Delete(ctx); err != nil {
			return fmt.Errorf("clear old WhatsApp link: %w", err)
		}
		device = container.NewDevice()
		fmt.Println("Cleared the saved WhatsApp link; waiting for a fresh QR scan.")
	}
	client := whatsmeow.NewClient(device, logger)
	c.client = client
	client.AddEventHandler(func(raw any) {
		switch event := raw.(type) {
		case *events.CallOffer:
			chat := event.From
			if !event.GroupJID.IsEmpty() {
				chat = event.GroupJID
			}
			c.forwardCall(ctx, chat, "Incoming WhatsApp call", onMessage)
		case *events.CallAccept:
			chat := event.From
			if !event.GroupJID.IsEmpty() {
				chat = event.GroupJID
			}
			c.forwardCall(ctx, chat, "WhatsApp call connected", onMessage)
		case *events.CallOfferNotice:
			chat := event.From
			if !event.GroupJID.IsEmpty() {
				chat = event.GroupJID
			}
			c.forwardCall(ctx, chat, "Incoming WhatsApp "+firstNonEmpty(event.Media, "voice")+" group call", onMessage)
		case *events.CallTerminate:
			chat := event.From
			if !event.GroupJID.IsEmpty() {
				chat = event.GroupJID
			}
			c.forwardCall(ctx, chat, "WhatsApp call ended", onMessage)
		case *events.HistorySync:
			if event.Data == nil {
				return
			}
			conversationCount := 0
			archivedCount := 0
			for _, conversation := range event.Data.GetConversations() {
				originalJID, err := types.ParseJID(conversation.GetID())
				if err != nil || !isPrivateChat(originalJID) && originalJID.Server != types.GroupServer {
					continue
				}
				jid, name := c.resolveChat(ctx, originalJID, conversation.GetName())
				chatJID := jid.String()
				c.rememberChat(chatJID, name)
				archived := make([]history.Message, 0, len(conversation.GetMessages()))
				for _, historical := range conversation.GetMessages() {
					parsed, err := client.ParseWebMessage(originalJID, historical.GetMessage())
					if err != nil || parsed.IsViewOnce {
						continue
					}
					text := messageText(parsed)
					if text == "" {
						if parsed.Info.Type == "" {
							continue
						}
						text = "[" + parsed.Info.Type + " message]"
					}
					sender := senderName(parsed.Info.Sender, parsed.Info.PushName)
					if parsed.Info.IsFromMe {
						sender = "You"
					}
					item := history.Message{
						ChatJID: chatJID, RequestChatJID: originalJID.String(), MessageID: parsed.Info.ID, SenderJID: parsed.Info.Sender.String(),
						Timestamp: parsed.Info.Timestamp, Chat: name, Sender: sender, Text: text, FromMe: parsed.Info.IsFromMe,
					}
					if kind := mediaKind(parsed.Message); kind != "" {
						if data, downloadErr := client.DownloadAny(ctx, parsed.Message); downloadErr == nil && len(data) <= maxTelegramUploadBytes {
							item.MediaKind = kind
							item.MediaName = mediaFileName(kind, mediaMIME(parsed.Message))
							item.MediaMIME = mediaMIME(parsed.Message)
							item.MediaData = data
						}
					}
					archived = append(archived, item)
				}
				if err := archive.SaveBatch(ctx, archived); err != nil {
					fmt.Printf("Could not archive WhatsApp history for %s: %v\n", chatJID, err)
					continue
				}
				conversationCount++
				archivedCount += len(archived)
			}
			fmt.Printf("WhatsApp history sync type=%s received %d conversations and archived %d supported messages.\n", event.Data.GetSyncType(), conversationCount, archivedCount)
			if event.Data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND {
				select {
				case c.historySyncEvents <- struct{}{}:
				default:
				}
			}
		case *events.Connected:
			fmt.Println("WhatsApp connected.")
			go func() {
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if err := c.RefreshChats(refreshCtx); err != nil {
					fmt.Printf("Could not load chats yet: %v\n", err)
					return
				}
				c.PrintChats()
				if onConnected != nil {
					onConnected()
				}
			}()
		case *events.Message:
			if event.Info.Chat.IsEmpty() {
				return
			}
			chat, name := c.resolveChat(ctx, event.Info.Chat, event.Info.PushName)
			chatJID := chat.String()
			if isPrivateChat(event.Info.Chat) {
				c.rememberChat(chatJID, name)
			}
			text := messageText(event)
			if text == "" {
				return
			}
			sender := senderName(event.Info.Sender, event.Info.PushName)
			if event.Info.IsFromMe {
				sender = "You"
			}
			message := Message{
				ChatJID: chatJID, MessageID: event.Info.ID, SenderJID: event.Info.Sender.String(),
				Timestamp: event.Info.Timestamp, Chat: name, Sender: sender, Text: text, FromMe: event.Info.IsFromMe, IsViewOnce: event.IsViewOnce,
			}
			if !message.IsViewOnce {
				if err := archive.Save(ctx, history.Message{
					ChatJID: message.ChatJID, RequestChatJID: event.Info.Chat.String(), MessageID: message.MessageID, SenderJID: message.SenderJID,
					Timestamp: message.Timestamp, Chat: message.Chat, Sender: message.Sender, Text: message.Text, FromMe: message.FromMe,
				}); err != nil {
					fmt.Printf("Could not archive WhatsApp message from %s: %v\n", chatJID, err)
				}
			}
			if onMessage != nil {
				enabled, allowed := c.shouldForward(message)
				if enabled && allowed {
					if mediaKind := mediaKind(event.Message); mediaKind != "" {
						mediaData, err := client.DownloadAny(ctx, event.Message)
						if err != nil {
							fmt.Printf("Could not download %s from %s: %v\n", mediaKind, chatJID, err)
							if message.Text == "" {
								message.Text = "[" + mediaKind + " unavailable]"
							}
						} else if len(mediaData) > maxTelegramUploadBytes {
							fmt.Printf("Skipping %s from %s: %d bytes exceeds Telegram upload limit\n", mediaKind, chatJID, len(mediaData))
							if message.Text == "" {
								message.Text = "[" + mediaKind + " exceeds Telegram upload limit]"
							}
						} else {
							message.MediaKind = mediaKind
							message.MediaData = mediaData
							message.MediaMIME = mediaMIME(event.Message)
							message.MediaName = mediaFileName(mediaKind, message.MediaMIME)
						}
					}
					if message.MediaKind != "" && !message.IsViewOnce {
						if err := archive.Save(ctx, history.Message{
							ChatJID: message.ChatJID, MessageID: message.MessageID, SenderJID: message.SenderJID,
							Timestamp: message.Timestamp, Chat: message.Chat, Sender: message.Sender, Text: message.Text, FromMe: message.FromMe,
							MediaKind: message.MediaKind, MediaName: message.MediaName, MediaMIME: message.MediaMIME, MediaData: message.MediaData,
						}); err != nil {
							fmt.Printf("Could not archive %s from %s: %v\n", message.MediaKind, chatJID, err)
						}
					}
					if err := onMessage(message); err != nil {
						fmt.Printf("Could not forward live message from %s: %v\n", chatJID, err)
					} else if !message.IsViewOnce {
						if err := archive.MarkForwarded(ctx, history.Message{ChatJID: message.ChatJID, MessageID: message.MessageID, SenderJID: message.SenderJID}); err != nil {
							fmt.Printf("Could not record forwarded message from %s: %v\n", chatJID, err)
						}
					}
				}
			}
		}
	})

	if device.ID == nil {
		for {
			qrChan, err := client.GetQRChannel(ctx)
			if err != nil {
				return fmt.Errorf("prepare WhatsApp QR login: %w", err)
			}
			if err := client.Connect(); err != nil {
				return fmt.Errorf("connect WhatsApp: %w", err)
			}
			fmt.Println("Scan this QR code from WhatsApp > Linked devices:")
			expired := false
			for item := range qrChan {
				switch item.Event {
				case whatsmeow.QRChannelEventCode:
					if err := displayPairingQR(item.Code); err != nil {
						return fmt.Errorf("display WhatsApp QR code: %w", err)
					}
				case whatsmeow.QRChannelEventError:
					return fmt.Errorf("WhatsApp QR login failed: %w", item.Error)
				case "success":
					fmt.Println("WhatsApp linked successfully.")
				case "timeout":
					fmt.Println("QR login timed out; requesting a fresh code.")
					expired = true
				case "err-scanned-without-multidevice":
					return fmt.Errorf("enable linked devices on the WhatsApp phone and scan again")
				case "err-client-outdated":
					return fmt.Errorf("WhatsApp client is outdated; update the bridge dependencies")
				}
			}
			if !expired {
				break
			}
		}
	} else if err := client.Connect(); err != nil {
		return fmt.Errorf("connect WhatsApp: %w", err)
	}

	<-ctx.Done()
	client.Disconnect()
	return nil
}

const maxTelegramUploadBytes = 49 * 1024 * 1024

func (c *Client) forwardCall(ctx context.Context, source types.JID, text string, onMessage func(Message) error) {
	if source.IsEmpty() || onMessage == nil {
		return
	}
	chat, name := c.resolveChat(ctx, source, "")
	message := Message{
		ChatJID: chat.String(), SenderJID: source.String(), Timestamp: time.Now(),
		Chat: name, Sender: "Call notification", Text: text,
	}
	enabled, allowed := c.shouldForward(message)
	if !enabled || !allowed {
		return
	}
	if err := onMessage(message); err != nil {
		fmt.Printf("Could not forward call notification for %s: %v\n", chat, err)
	}
}

func mediaKind(message *waE2E.Message) string {
	if message == nil {
		return ""
	}
	switch {
	case message.GetImageMessage() != nil:
		return "photo"
	case message.GetVideoMessage() != nil:
		if message.GetVideoMessage().GetGifPlayback() {
			return "animation"
		}
		return "video"
	case message.GetAudioMessage() != nil:
		if message.GetAudioMessage().GetPTT() {
			return "voice"
		}
		return "audio"
	case message.GetStickerMessage() != nil:
		return "sticker"
	case message.GetDocumentMessage() != nil:
		return "document"
	default:
		return ""
	}
}

func mediaMIME(message *waE2E.Message) string {
	if message == nil {
		return "application/octet-stream"
	}
	switch {
	case message.GetImageMessage() != nil:
		return message.GetImageMessage().GetMimetype()
	case message.GetVideoMessage() != nil:
		return message.GetVideoMessage().GetMimetype()
	case message.GetAudioMessage() != nil:
		return message.GetAudioMessage().GetMimetype()
	case message.GetStickerMessage() != nil:
		return message.GetStickerMessage().GetMimetype()
	case message.GetDocumentMessage() != nil:
		return message.GetDocumentMessage().GetMimetype()
	default:
		return "application/octet-stream"
	}
}

func mediaFileName(kind, mime string) string {
	extension := ".bin"
	switch mime {
	case "image/jpeg":
		extension = ".jpg"
	case "image/png":
		extension = ".png"
	case "image/webp":
		extension = ".webp"
	case "video/mp4":
		extension = ".mp4"
	case "audio/ogg", "audio/ogg; codecs=opus":
		extension = ".ogg"
	case "audio/mpeg":
		extension = ".mp3"
	case "application/pdf":
		extension = ".pdf"
	}
	return kind + extension
}

func (c *Client) shouldForward(message Message) (bool, bool) {
	if c.selects == nil || !c.selects.Enabled(message.ChatJID) {
		return false, false
	}
	since := c.selects.Since(message.ChatJID)
	if since == "" {
		return true, true
	}
	cutoff, err := time.ParseInLocation("2006-01-02", since, time.Local)
	return true, err == nil && !message.Timestamp.Before(cutoff)
}

func (c *Client) ReplaySince(ctx context.Context, chatJID, since string, onMessage func(Message) error) (int, error) {
	if c.archive == nil {
		return 0, fmt.Errorf("local history archive is not ready")
	}
	cutoff, err := time.ParseInLocation("2006-01-02", since, time.Local)
	if err != nil {
		return 0, fmt.Errorf("cutoff must use YYYY-MM-DD format")
	}
	if onMessage == nil {
		return 0, fmt.Errorf("message sender is not available")
	}
	fetchResult, err := c.fetchOlderHistory(ctx, chatJID, cutoff, nil)
	if err != nil {
		return 0, err
	}
	count := 0
	for {
		batch, err := c.archive.PendingSince(ctx, chatJID, cutoff, 500)
		if err != nil {
			return count, err
		}
		if len(batch) == 0 {
			if fetchResult.Detail != "" {
				return count, fmt.Errorf("WhatsApp history backfill: %s", fetchResult.Detail)
			}
			return count, nil
		}
		for _, archived := range batch {
			message := Message{
				ChatJID: archived.ChatJID, MessageID: archived.MessageID, SenderJID: archived.SenderJID,
				Timestamp: archived.Timestamp, Chat: archived.Chat, Sender: archived.Sender, Text: archived.Text, FromMe: archived.FromMe,
				MediaKind: archived.MediaKind, MediaName: archived.MediaName, MediaMIME: archived.MediaMIME, MediaData: archived.MediaData,
			}
			if err := onMessage(message); err != nil {
				return count, err
			}
			if err := c.archive.MarkForwarded(ctx, archived); err != nil {
				return count, err
			}
			count++
			select {
			case <-ctx.Done():
				return count, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func (c *Client) ReplayRange(ctx context.Context, chatJID, fromDate, toDate string, onMessage func(Message) error, onProgress history.ReplayProgress) (history.ReplayResult, error) {
	result := history.ReplayResult{}
	if c.archive == nil {
		return result, fmt.Errorf("local history archive is not ready")
	}
	from, err := time.ParseInLocation("2006-01-02", fromDate, time.Local)
	if err != nil {
		return result, fmt.Errorf("start date must use YYYY-MM-DD format")
	}
	toDay, err := time.ParseInLocation("2006-01-02", toDate, time.Local)
	if err != nil {
		return result, fmt.Errorf("end date must use YYYY-MM-DD format")
	}
	if toDay.Before(from) {
		return result, fmt.Errorf("end date must be the same as or later than the start date")
	}
	if onMessage == nil {
		return result, fmt.Errorf("message sender is not available")
	}
	until := toDay.AddDate(0, 0, 1)
	fetchResult, fetchErr := c.fetchOlderHistory(ctx, chatJID, from, onProgress)
	if fetchErr != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		fetchResult.Detail = fetchErr.Error()
	}
	result.ReachedStart = fetchResult.ReachedStart
	result.Detail = fetchResult.Detail
	result.Available, err = c.archive.CountRange(ctx, chatJID, from, until)
	if err != nil {
		return result, fmt.Errorf("count archived messages in the requested range: %w", err)
	}
	if onProgress != nil {
		onProgress(fmt.Sprintf("Backfill finished. %d messages are available in the selected range; sending them now.", result.Available))
	}

	var cursor *history.Message
	for {
		batch, err := c.archive.Range(ctx, chatJID, from, until, cursor, 1)
		if err != nil {
			return result, fmt.Errorf("read archived messages in the requested range: %w", err)
		}
		if len(batch) == 0 {
			return result, nil
		}
		for _, archived := range batch {
			message := Message{
				ChatJID: archived.ChatJID, MessageID: archived.MessageID, SenderJID: archived.SenderJID,
				Timestamp: archived.Timestamp, Chat: archived.Chat, Sender: archived.Sender, Text: archived.Text, FromMe: archived.FromMe,
				MediaKind: archived.MediaKind, MediaName: archived.MediaName, MediaMIME: archived.MediaMIME, MediaData: archived.MediaData,
			}
			if err := onMessage(message); err != nil {
				return result, err
			}
			result.Count++
			if onProgress != nil && result.Count%100 == 0 {
				onProgress(fmt.Sprintf("Sent %d of %d archived messages in the selected range.", result.Count, result.Available))
			}
			last := archived
			cursor = &last
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
}

func (c *Client) ContactsContacted(ctx context.Context, from, until time.Time) ([]history.Contact, error) {
	if c.archive == nil {
		return nil, fmt.Errorf("local history archive is not ready")
	}
	return c.archive.ContactsContacted(ctx, from, until)
}

type olderHistoryResult struct {
	ReachedStart bool
	Detail       string
}

func (c *Client) fetchOlderHistory(ctx context.Context, chatJID string, cutoff time.Time, onProgress history.ReplayProgress) (olderHistoryResult, error) {
	result := olderHistoryResult{}
	if c.client == nil || !c.client.IsLoggedIn() {
		result.Detail = "WhatsApp is disconnected; only history already present in this bridge was searched."
		return result, nil
	}
	if c.historySyncEvents == nil {
		c.historySyncEvents = make(chan struct{}, 8)
	}
	c.historyRequestMu.Lock()
	defer c.historyRequestMu.Unlock()

	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return result, fmt.Errorf("invalid chat ID %q: %w", chatJID, err)
	}
	for page := 0; ; page++ {
		oldest, found, err := c.archive.Oldest(ctx, chatJID)
		if err != nil {
			return result, fmt.Errorf("find history cursor: %w", err)
		}
		if !found {
			result.Detail = "No messages are archived for this contact yet, so this linked device has no message anchor for requesting older history."
			return result, nil
		}
		if !oldest.Timestamp.After(cutoff) {
			result.ReachedStart = true
			return result, nil
		}

		requestChat := chat
		if oldest.RequestChatJID != "" {
			requestChat, err = types.ParseJID(oldest.RequestChatJID)
			if err != nil {
				return result, fmt.Errorf("invalid archived history chat ID %q: %w", oldest.RequestChatJID, err)
			}
		} else if chat.Server == types.DefaultUserServer {
			if lid, lidErr := c.client.Store.LIDs.GetLIDForPN(ctx, chat); lidErr == nil && !lid.IsEmpty() {
				requestChat = lid
			}
		}
		anchor := &types.MessageInfo{
			MessageSource: types.MessageSource{Chat: requestChat, IsFromMe: oldest.FromMe},
			ID:            oldest.MessageID, Timestamp: oldest.Timestamp,
		}
		for len(c.historySyncEvents) > 0 {
			<-c.historySyncEvents
		}
		if page%10 == 0 && onProgress != nil {
			onProgress(fmt.Sprintf("Requesting WhatsApp history page %d for %s; current oldest message is %s.", page+1, c.chatName(chatJID), oldest.Timestamp.Format("2006-01-02 15:04:05")))
		}
		fmt.Printf("Requesting WhatsApp history page %d for %s before %s (%s).\n", page+1, chatJID, oldest.Timestamp.Format("2006-01-02 15:04:05"), oldest.MessageID)
		if _, err := c.client.SendPeerMessage(ctx, c.client.BuildHistorySyncRequest(anchor, 50)); err != nil {
			result.Detail = fmt.Sprintf("WhatsApp rejected the older-history request: %v", err)
			return result, nil
		}

		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
			result.Detail = fmt.Sprintf("WhatsApp did not return an older-history page within 30 seconds. The oldest archived message is from %s.", oldest.Timestamp.Format("2006-01-02"))
			return result, nil
		case <-c.historySyncEvents:
			timer.Stop()
		}

		updated, found, err := c.archive.Oldest(ctx, chatJID)
		if err != nil {
			return result, fmt.Errorf("check older WhatsApp history: %w", err)
		}
		if !found || !isOlderCursor(updated, oldest) {
			result.Detail = fmt.Sprintf("WhatsApp returned no older messages before %s. The oldest message currently archived for this contact is from %s.", cutoff.Format("2006-01-02"), oldest.Timestamp.Format("2006-01-02"))
			return result, nil
		}
	}
}

func isOlderCursor(candidate, current history.Message) bool {
	return candidate.Timestamp.Before(current.Timestamp)
}

func displayPairingQR(value string) error {
	if runtime.GOOS != "windows" {
		qrterminal.GenerateHalfBlock(value, qrterminal.L, os.Stdout)
		return nil
	}
	code, err := qr.Encode(value, qr.L)
	if err != nil {
		return err
	}
	path := filepath.Join("data", fmt.Sprintf("whatsapp-link-qr-%d.png", time.Now().UnixNano()))
	if err := os.WriteFile(path, code.PNG(), 0o600); err != nil {
		return err
	}
	if err := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", path).Start(); err != nil {
		fmt.Printf("Could not open QR image automatically (%v); showing it in the terminal instead.\n", err)
		qrterminal.GenerateHalfBlock(value, qrterminal.L, os.Stdout)
		return nil
	}
	fmt.Printf("Pairing QR opened as an image. If it does not appear, open %s manually.\n", path)
	return nil
}

func (c *Client) RefreshChats(ctx context.Context) error {
	if c.client == nil || !c.client.IsLoggedIn() {
		return fmt.Errorf("WhatsApp is not connected yet")
	}
	contacts, err := c.client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return fmt.Errorf("load contacts: %w", err)
	}
	chats := make(map[string]string, len(contacts))
	for jid, info := range contacts {
		resolved, name := c.resolveChat(ctx, jid, firstNonEmpty(info.FullName, info.FirstName, info.PushName, info.BusinessName))
		chats[resolved.String()] = name
	}
	for _, cachedChat := range c.Chats() {
		jid, err := types.ParseJID(cachedChat.JID)
		if err != nil {
			continue
		}
		resolved, resolvedName := c.resolveChat(ctx, jid, cachedChat.Name)
		if jid.Server == types.HiddenUserServer && resolved.Server != types.HiddenUserServer {
			delete(chats, jid.String())
		}
		chats[resolved.String()] = resolvedName
	}
	groups, err := c.client.GetJoinedGroups(ctx)
	if err != nil {
		return fmt.Errorf("load WhatsApp groups: %w", err)
	}
	for _, group := range groups {
		chats[group.JID.String()] = firstNonEmpty(group.Name, group.JID.String())
	}

	list := make([]Chat, 0, len(chats))
	for jid, name := range chats {
		list = append(list, Chat{JID: jid, Name: name})
	}
	sort.Slice(list, func(i, j int) bool {
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	c.mu.Lock()
	c.chats = list
	c.mu.Unlock()
	return nil
}

func (c *Client) Chats() []Chat {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Chat(nil), c.chats...)
}

func (c *Client) rememberChat(jid, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.chats {
		if c.chats[i].JID == jid {
			if name != "" && c.chats[i].Name == c.chats[i].JID {
				c.chats[i].Name = name
			}
			return
		}
	}
	c.chats = append(c.chats, Chat{JID: jid, Name: name})
	sort.Slice(c.chats, func(i, j int) bool {
		return strings.ToLower(c.chats[i].Name) < strings.ToLower(c.chats[j].Name)
	})
}

func isPrivateChat(jid types.JID) bool {
	return jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer || jid.Server == types.LegacyUserServer
}

func normalizeChatJID(jid types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server == types.LegacyUserServer {
		jid.Server = types.DefaultUserServer
	}
	return jid
}

func (c *Client) resolveChat(ctx context.Context, jid types.JID, preferredName string) (types.JID, string) {
	jid = normalizeChatJID(jid)
	originalJID := jid
	name := strings.TrimSpace(preferredName)
	if originalJID.Server == types.HiddenUserServer && name == originalJID.User {
		name = ""
	}
	if jid.Server == types.HiddenUserServer && c.client != nil {
		if phoneJID, err := c.client.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !phoneJID.IsEmpty() {
			jid = normalizeChatJID(phoneJID)
		}
	}
	if c.client != nil && (jid.Server == types.DefaultUserServer || jid.Server == types.LegacyUserServer) {
		if info, err := c.client.Store.Contacts.GetContact(ctx, jid); err == nil {
			name = firstNonEmpty(info.FullName, info.FirstName, info.PushName, info.BusinessName, name)
		}
	}
	if name == "" && originalJID.Server == types.HiddenUserServer && c.client != nil {
		if info, err := c.client.Store.Contacts.GetContact(ctx, originalJID); err == nil {
			name = firstNonEmpty(info.FullName, info.FirstName, info.PushName, info.BusinessName)
		}
	}
	fallback := jid.User
	if jid.Server == types.HiddenUserServer {
		fallback = "Unknown private chat"
	}
	return jid, firstNonEmpty(name, fallback)
}

func (c *Client) PrintChats() {
	chats := c.Chats()
	fmt.Printf("Available contacts and groups (%d):\n", len(chats))
	for _, chat := range chats {
		fmt.Printf("  %s  [%s]\n", chat.Name, chat.JID)
	}
	if len(chats) == 0 {
		fmt.Println("  No contacts or groups were synced yet. Use /chats in Telegram to refresh later.")
	}
}

func (c *Client) chatName(jid string) string {
	for _, chat := range c.Chats() {
		if chat.JID == jid {
			return chat.Name
		}
	}
	return jid
}

func messageText(event *events.Message) string {
	message := event.Message
	if message == nil {
		return ""
	}
	if text := message.GetConversation(); text != "" {
		return text
	}
	if text := message.GetExtendedTextMessage().GetText(); text != "" {
		return text
	}
	if caption := message.GetImageMessage().GetCaption(); caption != "" {
		return "[Image] " + caption
	}
	if caption := message.GetVideoMessage().GetCaption(); caption != "" {
		return "[Video] " + caption
	}
	if caption := message.GetDocumentMessage().GetCaption(); caption != "" {
		return "[Document] " + caption
	}
	if kind := mediaKind(message); kind != "" {
		return "[" + kind + " message]"
	}
	if event.Info.Type != "" {
		return "[" + event.Info.Type + " message]"
	}
	return ""
}

func senderName(jid types.JID, pushName string) string {
	if pushName != "" {
		return pushName
	}
	if !jid.IsEmpty() {
		return jid.String()
	}
	return "Unknown sender"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "Unknown chat"
}
