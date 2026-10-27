package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/local/wa-telegram-bridge/internal/history"
	"github.com/local/wa-telegram-bridge/internal/selection"
	"github.com/local/wa-telegram-bridge/internal/telegram"
	"github.com/local/wa-telegram-bridge/internal/timezone"
	"github.com/local/wa-telegram-bridge/internal/whatsapp"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-service" {
		if err := runWindowsService(); err != nil {
			log.Fatal(err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runBridge(ctx); err != nil {
		log.Fatal(err)
	}
}

func runBridge(parent context.Context) error {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is required; create a bot with BotFather and set the token as an environment variable")
	}
	ownerID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("TELEGRAM_OWNER_ID")), 10, 64)
	if err != nil || ownerID <= 0 {
		return fmt.Errorf("TELEGRAM_OWNER_ID must be your numeric Telegram user ID")
	}

	if err := os.MkdirAll("data", 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	selectedChats, err := selection.Open("data/selected-chats.json")
	if err != nil {
		return fmt.Errorf("load selected chats: %w", err)
	}

	ctx, stop := context.WithCancel(parent)
	defer stop()
	wa := whatsapp.New(selectedChats)
	var bot *telegram.Bot
	bot = telegram.New(token, ownerID, selectedChats, func() []telegram.Chat {
		chats := wa.Chats()
		result := make([]telegram.Chat, 0, len(chats))
		for _, chat := range chats {
			result = append(result, telegram.Chat{
				JID:     chat.JID,
				Name:    chat.Name,
				Enabled: selectedChats.Enabled(chat.JID),
				Since:   selectedChats.Since(chat.JID),
			})
		}
		return result
	}, wa.RefreshChats, func(ctx context.Context, jid, since string) (int, error) {
		if err := selectedChats.SetSince(jid, since); err != nil {
			return 0, err
		}
		if err := selectedChats.SetEnabled(jid, true); err != nil {
			return 0, err
		}
		return wa.ReplaySince(ctx, jid, since, func(message whatsapp.Message) error {
			return bot.SendForwarded(ctx, formatForwardedMessage(message))
		})
	}, func(ctx context.Context, jid, fromDate, toDate string, progress history.ReplayProgress) (history.ReplayResult, error) {
		return wa.ReplayRange(ctx, jid, fromDate, toDate, func(message whatsapp.Message) error {
			return bot.SendForwarded(ctx, formatForwardedMessage(message))
		}, progress)
	}, func(ctx context.Context, from, until time.Time) ([]history.Contact, error) {
		return wa.ContactsContacted(ctx, from, until)
	}, func(ctx context.Context, from, until time.Time) ([]history.Contact, error) {
		return wa.ContactsMessagedByMe(ctx, from, until)
	}, wa.RefreshCallHistory, func(ctx context.Context, from, until time.Time) ([]history.CallContact, error) {
		return wa.CallsContacted(ctx, from, until)
	})
	go func() {
		if err := bot.Run(ctx); err != nil {
			log.Printf("Telegram bot stopped: %v", err)
			stop()
		}
	}()
	fmt.Println("Telegram bot started. Open your bot and send /start, then /chats.")

	err = wa.Run(ctx, func(message whatsapp.Message) error {
		return bot.SendForwarded(ctx, formatForwardedMessage(message))
	}, func() {
		if err := bot.SendText(ctx, "WhatsApp connected. Use /chats to select which chats to forward."); err != nil {
			log.Printf("notify Telegram owner: %v", err)
		}
	})
	if err != nil {
		return err
	}
	return nil
}

func formatForwardedMessage(message whatsapp.Message) telegram.ForwardedMessage {
	stamp := message.Timestamp.In(timezone.Kenya).Format("2006-01-02 15:04")
	return telegram.ForwardedMessage{
		Chat: message.Chat, Sender: message.Sender, Timestamp: stamp, Text: message.Text,
		MediaKind: message.MediaKind, MediaName: message.MediaName, MediaMIME: message.MediaMIME, MediaData: message.MediaData,
		IsViewOnce: message.IsViewOnce,
	}
}
