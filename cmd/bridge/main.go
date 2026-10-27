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

	"github.com/local/wa-telegram-bridge/internal/selection"
	"github.com/local/wa-telegram-bridge/internal/telegram"
	"github.com/local/wa-telegram-bridge/internal/whatsapp"
)

func main() {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is required; create a bot with BotFather and set the token as an environment variable")
	}
	ownerID, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("TELEGRAM_OWNER_ID")), 10, 64)
	if err != nil || ownerID <= 0 {
		log.Fatal("TELEGRAM_OWNER_ID must be your numeric Telegram user ID")
	}

	if err := os.MkdirAll("data", 0o700); err != nil {
		log.Fatalf("create data directory: %v", err)
	}
	selectedChats, err := selection.Open("data/selected-chats.json")
	if err != nil {
		log.Fatalf("load selected chats: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
		log.Fatal(err)
	}
}

func formatForwardedMessage(message whatsapp.Message) telegram.ForwardedMessage {
	stamp := message.Timestamp.Local().Format("2006-01-02 15:04")
	return telegram.ForwardedMessage{
		Chat: message.Chat, Sender: message.Sender, Timestamp: stamp, Text: message.Text,
		MediaKind: message.MediaKind, MediaName: message.MediaName, MediaMIME: message.MediaMIME, MediaData: message.MediaData,
	}
}
