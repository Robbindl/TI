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
	"sync"
	"time"
	"unicode/utf16"

	"github.com/local/wa-telegram-bridge/internal/history"
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
	token        string
	ownerID      int64
	selection    *selection.Store
	chats        func() []Chat
	refresh      func(context.Context) error
	setSince     func(context.Context, string, string) (int, error)
	replayRange  func(context.Context, string, string, string, history.ReplayProgress) (history.ReplayResult, error)
	http         *http.Client
	pendingJID   string
	pendingPage  int
	pendingRange bool
	pendingFrom  string
	historyMu    sync.Mutex
	historyBusy  bool
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
	OK         bool            `json:"ok"`
	Result     json.RawMessage `json:"result"`
	Error      string          `json:"description"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func New(token string, ownerID int64, store *selection.Store, chats func() []Chat, refresh func(context.Context) error, setSince func(context.Context, string, string) (int, error), replayRange func(context.Context, string, string, string, history.ReplayProgress) (history.ReplayResult, error)) *Bot {
	return &Bot{
		token:       token,
		ownerID:     ownerID,
		selection:   store,
		chats:       chats,
		refresh:     refresh,
		setSince:    setSince,
		replayRange: replayRange,
		http:        &http.Client{Timeout: 65 * time.Second},
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
		return b.sendLongText(ctx, caption)
	}
	method, field := mediaEndpoint(message.MediaKind)
	if method == "" {
		return b.sendLongText(ctx, caption+"\n[Unsupported attachment]")
	}
	if message.MediaKind == "sticker" {
		if err := b.sendLongText(ctx, caption); err != nil {
			return err
		}
		caption = ""
	}
	if len([]rune(caption)) > 1024 {
		if err := b.sendLongText(ctx, caption); err != nil {
			return err
		}
		caption = message.Chat + "\n" + message.Timestamp + "\n" + message.Sender + " [attached " + message.MediaKind + "]"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("chat_id", strconv.FormatInt(b.ownerID, 10)); err != nil {
		return err
	}
	if caption != "" {
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
	_, err = b.request(ctx, method, func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
		if err == nil {
			request.Header.Set("Content-Type", writer.FormDataContentType())
		}
		return request, err
	})
	return err
}

func (b *Bot) sendLongText(ctx context.Context, text string) error {
	for _, chunk := range splitTelegramText(text, 4000) {
		if err := b.SendText(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

func splitTelegramText(text string, maxUnits int) []string {
	var chunks []string
	var current strings.Builder
	units := 0
	for _, character := range text {
		characterUnits := utf16.RuneLen(character)
		if characterUnits < 0 {
			characterUnits = 1
		}
		if units+characterUnits > maxUnits && current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
			units = 0
		}
		current.WriteRune(character)
		units += characterUnits
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
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
			dateInput := strings.TrimSpace(item.Message.Text)
			if strings.EqualFold(dateInput, "/cancel") {
				b.clearPendingDate()
				_ = b.sendText(ctx, b.ownerID, "History request cancelled.", inlineKeyboard{})
				return
			}
			date, err := time.ParseInLocation("2006-01-02", dateInput, time.Local)
			if err != nil {
				_ = b.sendText(ctx, b.ownerID, "Send a date in YYYY-MM-DD format, for example 2026-03-15. Send /cancel to stop.", inlineKeyboard{})
				return
			}
			if b.pendingRange && b.pendingFrom == "" {
				b.pendingFrom = date.Format("2006-01-02")
				_ = b.sendText(ctx, b.ownerID, "Now send the inclusive end date in YYYY-MM-DD format. Send /cancel to stop.", inlineKeyboard{})
				return
			}
			if b.pendingRange {
				fromDate := b.pendingFrom
				from, _ := time.ParseInLocation("2006-01-02", fromDate, time.Local)
				if date.Before(from) {
					_ = b.sendText(ctx, b.ownerID, "The end date must be the same as or later than the start date. Send the end date again, or /cancel.", inlineKeyboard{})
					return
				}
				jid, page := b.pendingJID, b.pendingPage
				toDate := date.Format("2006-01-02")
				b.clearPendingDate()
				if b.replayRange == nil {
					_ = b.sendText(ctx, b.ownerID, "History retrieval is unavailable right now.", inlineKeyboard{})
					return
				}
				b.historyMu.Lock()
				if b.historyBusy {
					b.historyMu.Unlock()
					_ = b.sendText(ctx, b.ownerID, "A history request is already running. Wait for its result, then start another range.", inlineKeyboard{})
					return
				}
				b.historyBusy = true
				b.historyMu.Unlock()
				_ = b.sendText(ctx, b.ownerID, fmt.Sprintf("Retrieving %s through %s for this chat. Keep WhatsApp open on your primary phone while older history is requested.", fromDate, toDate), inlineKeyboard{})
				go b.runHistoryRange(ctx, jid, fromDate, toDate, page)
				return
			}
			jid, page := b.pendingJID, b.pendingPage
			b.clearPendingDate()
			if b.setSince == nil {
				_ = b.sendText(ctx, b.ownerID, "History replay is unavailable right now.", inlineKeyboard{})
				return
			}
			count, err := b.setSince(ctx, jid, date.Format("2006-01-02"))
			if err != nil {
				_ = b.sendText(ctx, b.ownerID, fmt.Sprintf("The cutoff is saved and live forwarding remains enabled. Replayed %d available messages. WhatsApp history retrieval stopped: %s", count, err.Error()), inlineKeyboard{})
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
			b.pendingRange = false
			b.pendingFrom = ""
			_ = b.sendText(ctx, query.Message.Chat.ID, "Set the earliest date for live forwarding and import available history from that date. Send YYYY-MM-DD, or /cancel.", inlineKeyboard{})
		}
	} else if len(parts) == 3 && parts[0] == "range" {
		page, pageErr := strconv.Atoi(parts[2])
		if pageErr == nil {
			b.pendingJID = parts[1]
			b.pendingPage = page
			b.pendingRange = true
			b.pendingFrom = ""
			_ = b.sendText(ctx, query.Message.Chat.ID, "Send the inclusive start date for this chat as YYYY-MM-DD. Then I’ll ask for the inclusive end date. Send /cancel to stop.", inlineKeyboard{})
		}
	}
	b.answerCallback(ctx, query.ID)
}

func (b *Bot) clearPendingDate() {
	b.pendingJID = ""
	b.pendingPage = 0
	b.pendingRange = false
	b.pendingFrom = ""
}

func (b *Bot) runHistoryRange(ctx context.Context, jid, fromDate, toDate string, page int) {
	defer func() {
		b.historyMu.Lock()
		b.historyBusy = false
		b.historyMu.Unlock()
	}()
	result, err := b.replayRange(ctx, jid, fromDate, toDate, func(progress string) {
		if err := b.sendText(ctx, b.ownerID, progress, inlineKeyboard{}); err != nil {
			fmt.Printf("Could not send history progress to Telegram: %v\n", err)
		}
	})
	message := fmt.Sprintf("History %s through %s: sent %d messages; %d messages are available in the local archive for this range.", fromDate, toDate, result.Count, result.Available)
	switch {
	case err != nil:
		message += " Retrieval stopped after the messages above: " + err.Error()
	case result.ReachedStart:
		message += " The archive reached the requested start date."
	case result.Detail != "":
		message += " Older history is incomplete: " + result.Detail
	default:
		message += " Older history could not be confirmed as available from WhatsApp."
	}
	if err := b.sendText(ctx, b.ownerID, message, inlineKeyboard{}); err != nil {
		fmt.Printf("Could not report history range result to Telegram: %v\n", err)
	}
	b.showChats(ctx, b.ownerID, 0, page)
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
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, []inlineButton{{
			Text:         "History range | " + label,
			CallbackData: "range|" + chat.JID + "|" + strconv.Itoa(page),
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
	text := fmt.Sprintf("Forwarding selection (%d chats, page %d/%d):\nON/OFF controls live forwarding. Since sets a forwarding cutoff. History range sends an inclusive date range again whenever requested.", len(chats), page+1, totalPages)
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
	apiResult, err := b.request(ctx, method, func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
		}
		return request, err
	})
	if err != nil {
		return err
	}
	if result != nil {
		return json.Unmarshal(apiResult.Result, result)
	}
	return nil
}

func (b *Bot) request(ctx context.Context, method string, buildRequest func() (*http.Request, error)) (apiResponse, error) {
	for {
		request, err := buildRequest()
		if err != nil {
			return apiResponse{}, err
		}
		response, err := b.http.Do(request)
		if err != nil {
			return apiResponse{}, err
		}
		var apiResult apiResponse
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiResult)
		statusCode := response.StatusCode
		_ = response.Body.Close()
		if decodeErr != nil {
			return apiResponse{}, decodeErr
		}
		if statusCode == http.StatusTooManyRequests && apiResult.Parameters.RetryAfter > 0 {
			timer := time.NewTimer(time.Duration(apiResult.Parameters.RetryAfter) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return apiResponse{}, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if statusCode < 200 || statusCode >= 300 || !apiResult.OK {
			return apiResponse{}, fmt.Errorf("Telegram API %s: %s", method, apiResult.Error)
		}
		return apiResult, nil
	}
}

func clip(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}
	return string([]rune(value)[:limit-1]) + "…"
}
