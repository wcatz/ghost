package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/wcatz/ghost/internal/mdv2"
	"github.com/wcatz/ghost/internal/voice"
)

// STTProvider transcribes audio to text.
type STTProvider interface {
	Transcribe(ctx context.Context, audio []byte) (string, error)
}

// SetSTT configures the speech-to-text provider for voice messages.
func (tb *Bot) SetSTT(stt STTProvider) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.stt = stt
}

// handleVoice processes incoming Telegram voice messages.
// Downloads the OGG, converts to WAV, transcribes, and replies with the text.
func (tb *Bot) handleVoice(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.Voice == nil {
		return
	}
	if !tb.allowedIDs[update.Message.From.ID] {
		return
	}

	tb.mu.Lock()
	stt := tb.stt
	tb.mu.Unlock()

	if stt == nil {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Voice transcription is not configured. Set assemblyai_api_key or install whisper.",
		})
		return
	}

	// Send typing indicator.
	b.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: update.Message.Chat.ID,
		Action: models.ChatActionTyping,
	})

	// Download the voice file from Telegram.
	file, err := b.GetFile(ctx, &bot.GetFileParams{FileID: update.Message.Voice.FileID})
	if err != nil {
		tb.logger.Error("get voice file", "error", err)
		return
	}

	fileURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", tb.token, file.FilePath)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(fileURL)
	if err != nil {
		tb.logger.Error("download voice file", "error", err)
		return
	}
	defer resp.Body.Close()

	oggData, err := io.ReadAll(io.LimitReader(resp.Body, 25*1024*1024)) // 25MB limit
	if err != nil {
		tb.logger.Error("read voice file", "error", err)
		return
	}

	// Convert OGG to WAV.
	wavData, err := voice.OGGToWAV(ctx, oggData)
	if err != nil {
		tb.logger.Error("transcode voice", "error", err)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Failed to process voice message. Is ffmpeg installed?",
		})
		return
	}

	// Transcribe.
	text, err := stt.Transcribe(ctx, wavData)
	if err != nil {
		tb.logger.Error("transcribe voice", "error", err)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   fmt.Sprintf("Transcription failed: %v", err),
		})
		return
	}

	if text == "" {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: update.Message.Chat.ID,
			Text:   "Could not detect any speech in the voice message.",
		})
		return
	}

	// Reply with the transcription.
	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: update.Message.Chat.ID,
		Text:   fmt.Sprintf("🎤 *Transcription:*\n%s", mdv2.Esc(text)),
		ParseMode: models.ParseModeMarkdown,
	})

	tb.logger.Info("voice message transcribed", "chars", len(text))
}
