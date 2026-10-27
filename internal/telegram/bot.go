package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/local/wa-telegram-bridge/internal/selection"
)

const chatsPerPage = 8

type Chat struct {
	JID     string
	Name    string
	Enabled bool
	Since   string
}

type ForwardedMessage struct {
	Chat      string
	Sender    string
	Timestamp string
	Text      string
	MediaKind string
	MediaName string
	MediaMIME string
	MediaData []byte
}

type Bot struct {
	token       string
	ownerID     int64
	selection   *selection.Store
	chats       func() []Chat
	refresh     func(context.Context) error
	setSince    func(context.Context, string, string) (int, error)
	http        *http.Client
	pendingJID  string
	pendingPage int
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message"`
	CallbackQuery *callbackQuery `json:"callback_query"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	From      user   `json:"from"`
	Chat      user   `json:"chat"`
	Text      string `json:"text"`
}

type user struct {
	ID int64 `json:"id"`
}

type callbackQuery struct {
	ID      string   `json:"id"`
	From    user     `json:"from"`
	Message *message `json:"message"`
	Data    string   `json:"data"`
}

type inlineKeyboard struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type apiResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"description"`
}

func New(token string, ownerID int64, store *selection.Store, chats func() []Chat, refresh func(context.Context) error, setSince func(context.Context, string, string) (int, error)) *Bot {
	return &Bot{
		token:     token,
		ownerID:   ownerID,
		selection: store,
		chats:     chats,
		refresh:   refresh,
		setSince:  setSince,
		http:      &http.Client{Timeout: 65 * time.Second},
	}
}

func (b *Bot) Run(ctx context.Context) error {
	var offset int64
	for {
		if ctx.Err() != nil {
			return nil
		}
		var updates []update
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         50,
			"allowed_updates": []string{"message", "callback_query"},
		}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("Telegram polling error: %v\n", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, item := range updates {
			offset = item.UpdateID + 1
			b.handleUpdate(ctx, item)
		}
	}
}

func (b *Bot) SendText(ctx context.Context, text string) error {
	return b.sendText(ctx, b.ownerID, text, inlineKeyboard{})
}

func (b *Bot) SendForwarded(ctx context.Context, message ForwardedMessage) error {
	caption := message.Chat + "\n" + message.Timestamp + "\n" + message.Sender
	if strings.TrimSpace(message.Text) != "" {
		caption += ": " + message.Text
	}
	if len(message.MediaData) == 0 || message.MediaKind == "" {
		return b.SendText(ctx, caption)
	}
	method, field := mediaEndpoint(message.MediaKind)
	if method == "" {
		return b.SendText(ctx, caption+"\n[Unsupported attachment]")
	}
	if message.MediaKind == "sticker" {
		if err := b.SendText(ctx, caption); err != nil {
			return err
		}
		caption = ""
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(b.ownerID, 10)); err != nil {
		return err
	}
	if caption != "" {
		if len([]rune(caption)) > 1024 {
			caption = string([]rune(caption)[:1021]) + "..."
		}
		if err := writer.WriteField("caption", caption); err != nil {
			return err
		}
	}
	fileName := message.MediaName
	if fileName == "" {
		fileName = message.MediaKind + ".bin"
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": field, "filename": fileName}))
	header.Set("Content-Type", message.MediaMIME)
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(message.MediaData); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	endpoint := "https://api.telegram.org/bot" + url.PathEscape(b.token) + "/" + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := b.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var apiResult apiResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiResult); err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !apiResult.OK {
		return fmt.Errorf("Telegram API %s: %s", method, apiResult.Error)
	}
	return nil
}

func mediaEndpoint(kind string) (method, field string) {
	switch kind {
	case "photo":
		return "sendPhoto", "photo"
	case "video":
		return "sendVideo", "video"
	case "animation":
		return "sendAnimation", "animation"
	case "voice":
		return "sendVoice", "voice"
	case "audio":
		return "sendAudio", "audio"
	case "sticker":
		return "sendSticker", "sticker"
	case "document":
		return "sendDocument", "document"
	default:
		return "", ""
	}
}

func (b *Bot) handleUpdate(ctx context.Context, item update) {
	if item.Message != nil {
		if item.Message.From.ID != b.ownerID || item.Message.Chat.ID != b.ownerID {
			return
		}
		if b.pendingJID != "" {
			date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(item.Message.Text), time.Local)
			if err != nil {
				_ = b.sendText(ctx, b.ownerID, "Send a date in YYYY-MM-DD format, for example 2026-03-15.", inlineKeyboard{})
				return
			}
			jid, page := b.pendingJID, b.pendingPage
			b.pendingJID = ""
			if b.setSince == nil {
				_ = b.sendText(ctx, b.ownerID, "History replay is unavailable right now.", inlineKeyboard{})
				return
			}
			count, err := b.setSince(ctx, jid, date.Format("2006-01-02"))
			if err != nil {
				_ = b.sendText(ctx, b.ownerID, "The cutoff is saved and live forwarding remains enabled, but history replay stopped: "+err.Error(), inlineKeyboard{})
				return
			}
			_ = b.sendText(ctx, b.ownerID, fmt.Sprintf("Cutoff set to %s. Replayed %d available messages. New messages from this date onward will also forward.", date.Format("2006-01-02"), count), inlineKeyboard{})
			b.showChats(ctx, b.ownerID, 0, page)
			return
		}
		command := strings.Fields(item.Message.Text)
		if len(command) == 0 {
			return
		}
		switch strings.SplitN(command[0], "@", 2)[0] {
		case "/start", "/help":
			_ = b.sendText(ctx, b.ownerID, "WhatsApp bridge is ready. Use /chats to choose which chats forward here.", inlineKeyboard{})
		case "/chats":
			if b.refresh != nil {
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := b.refresh(refreshCtx)
				cancel()
				if err != nil {
					fmt.Printf("Could not refresh WhatsApp chats: %v\n", err)
					_ = b.sendText(ctx, b.ownerID, "Chat refresh failed; showing the last available list. Try /chats again shortly.", inlineKeyboard{})
				}
			}
			b.showChats(ctx, b.ownerID, 0, 0)
		}
		return
	}
	if item.CallbackQuery == nil || item.CallbackQuery.From.ID != b.ownerID || item.CallbackQuery.Message == nil || item.CallbackQuery.Message.Chat.ID != b.ownerID {
		return
	}
	query := item.CallbackQuery
	parts := strings.Split(query.Data, "|")
	if len(parts) == 2 && parts[0] == "page" {
		page, err := strconv.Atoi(parts[1])
		if err == nil && page >= 0 {
			b.showChats(ctx, query.Message.Chat.ID, query.Message.MessageID, page)
		}
		b.answerCallback(ctx, query.ID)
		return
	}
	if len(parts) == 3 && parts[0] == "toggle" {
		page, pageErr := strconv.Atoi(parts[2])
		if pageErr == nil {
			if enabled, err := b.selection.Toggle(parts[1]); err != nil {
				_ = b.sendText(ctx, b.ownerID, "Could not save chat selection: "+err.Error(), inlineKeyboard{})
			} else {
				state := "disabled"
				if enabled {
					state = "enabled"
				}
				fmt.Printf("Telegram owner %d %s forwarding for %s\n", b.ownerID, state, parts[1])
				b.showChats(ctx, query.Message.Chat.ID, query.Message.MessageID, page)
			}
		}
	} else if len(parts) == 3 && parts[0] == "since" {
		page, pageErr := strconv.Atoi(parts[2])
		if pageErr == nil {
			b.pendingJID = parts[1]
			b.pendingPage = page
			_ = b.sendText(ctx, query.Message.Chat.ID, "Choose the earliest date to import for this chat. Send it as YYYY-MM-DD. WhatsApp can only provide history it has synced to this linked device.", inlineKeyboard{})
		}
	}
	b.answerCallback(ctx, query.ID)
}

func (b *Bot) showChats(ctx context.Context, chatID, messageID int64, page int) {
	chats := b.chats()
	if len(chats) == 0 {
		_ = b.sendText(ctx, chatID, "No chats are available yet. Wait for WhatsApp to connect, then try /chats again.", inlineKeyboard{})
		return
	}
	totalPages := (len(chats) + chatsPerPage - 1) / chatsPerPage
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * chatsPerPage
	end := min(start+chatsPerPage, len(chats))
	keyboard := inlineKeyboard{}
	for _, chat := range chats[start:end] {
		icon := "OFF"
		if chat.Enabled {
			icon = "ON"
		}
		label := clip(chat.Name, 28)
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []inlineButton{{
			Text:         icon + " | " + label,
			CallbackData: "toggle|" + chat.JID + "|" + strconv.Itoa(page),
		}, {
			Text:         "Since " + cutoffLabel(chat.Since),
			CallbackData: "since|" + chat.JID + "|" + strconv.Itoa(page),
		}})
	}
	var navigation []inlineButton
	if page > 0 {
		navigation = append(navigation, inlineButton{Text: "< Previous", CallbackData: "page|" + strconv.Itoa(page-1)})
	}
	if page+1 < totalPages {
		navigation = append(navigation, inlineButton{Text: "Next >", CallbackData: "page|" + strconv.Itoa(page+1)})
	}
	if len(navigation) > 0 {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, navigation)
	}
	text := fmt.Sprintf("Forwarding selection (%d chats, page %d/%d):\nTap ON/OFF to toggle forwarding, or Since to import history from a date.", len(chats), page+1, totalPages)
	if messageID == 0 {
		_ = b.sendText(ctx, chatID, text, keyboard)
		return
	}
	_ = b.call(ctx, "editMessageText", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"text":         text,
		"reply_markup": keyboard,
	}, nil)
}

func cutoffLabel(since string) string {
	if since == "" {
		return "date"
	}
	return since
}

func (b *Bot) sendText(ctx context.Context, chatID int64, text string, keyboard inlineKeyboard) error {
	payload := map[string]any{"chat_id": chatID, "text": text}
	if len(keyboard.InlineKeyboard) > 0 {
		payload["reply_markup"] = keyboard
	}
	return b.call(ctx, "sendMessage", payload, nil)
}

func (b *Bot) answerCallback(ctx context.Context, id string) {
	_ = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id}, nil)
}

func (b *Bot) call(ctx context.Context, method string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := "https://api.telegram.org/bot" + url.PathEscape(b.token) + "/" + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := b.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var apiResult apiResponse
	if err := json.NewDecoder(response.Body).Decode(&apiResult); err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !apiResult.OK {
		return fmt.Errorf("Telegram API %s: %s", method, apiResult.Error)
	}
	if result != nil {
		return json.Unmarshal(apiResult.Result, result)
	}
	return nil
}

func clip(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}
	return string([]rune(value)[:limit-1]) + "…"
}
