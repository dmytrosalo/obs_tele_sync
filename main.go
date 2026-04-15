package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

var (
	driveService  *drive.Service
	rootFolderID  string
	inboxFolderID string
	attFolderID   string
	tgAttFolderID string
	allowedUserID int64
)

// --- Media group buffering ---

type mediaGroupEntry struct {
	msgs   []*models.Message
	timer  *time.Timer
	chatID int64
}

var (
	mediaGroups   = make(map[string]*mediaGroupEntry)
	mediaGroupsMu sync.Mutex
)

const mediaGroupWait = 1500 * time.Millisecond

func main() {
	token := mustEnv("BOT_TOKEN")
	rootFolderID = mustEnv("GDRIVE_FOLDER_ID")
	credsFile := envOr("CREDENTIALS_FILE", "credentials.json")
	tokenFile := envOr("TOKEN_FILE", "token.json")

	if v := os.Getenv("ALLOWED_USER_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			log.Fatalf("invalid ALLOWED_USER_ID: %v", err)
		}
		allowedUserID = id
	}

	// Init Google Drive with OAuth2
	client, err := getOAuthClient(credsFile, tokenFile)
	if err != nil {
		log.Fatalf("google drive auth: %v", err)
	}
	driveService, err = drive.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("google drive init: %v", err)
	}

	// Ensure folders exist
	inboxFolderID = mustGetOrCreateFolder(rootFolderID, "inbox")
	attFolderID = mustGetOrCreateFolder(rootFolderID, "attachments")
	tgAttFolderID = mustGetOrCreateFolder(attFolderID, "tg")
	log.Printf("inbox=%s attachments/tg=%s", inboxFolderID, tgAttFolderID)

	// Init bot
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	b, err := bot.New(token, bot.WithDefaultHandler(router))
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}

	log.Println("bot started")
	b.Start(ctx)
}

// --- Title generation ---

func makeTitle(msg *models.Message, content string, kind string) string {
	fwd := forwardInfo(msg)
	summary := truncate(firstLine(content), 50)

	switch {
	case fwd != "" && summary != "":
		return fmt.Sprintf("[tg] %s - %s", fwd, summary)
	case fwd != "":
		return fmt.Sprintf("[tg] %s", fwd)
	case summary != "":
		return fmt.Sprintf("[tg] %s", summary)
	default:
		return fmt.Sprintf("[tg] %s %s", kind, time.Now().Format("2006-01-02"))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	return s
}

func truncate(s string, maxChars int) string {
	if utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxChars]) + "..."
}

func sanitizeFolderName(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return r.Replace(name)
}

// --- Router ---

func router(ctx context.Context, b *bot.Bot, upd *models.Update) {
	if upd.Message == nil {
		return
	}
	msg := upd.Message

	// Auth check
	if allowedUserID != 0 && msg.From.ID != allowedUserID {
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "⛔ Немає доступу.",
		})
		return
	}

	// Media group handling
	if msg.MediaGroupID != "" {
		bufferMediaGroup(ctx, b, msg)
		return
	}

	var err error
	switch {
	case msg.Voice != nil:
		err = handleVoice(ctx, b, msg)
	case msg.VideoNote != nil:
		err = handleVideoNote(ctx, b, msg)
	case msg.Photo != nil:
		err = handlePhoto(ctx, b, msg)
	case msg.Document != nil:
		err = handleDocument(ctx, b, msg)
	case msg.Video != nil:
		err = handleVideo(ctx, b, msg)
	case msg.Text != "":
		err = handleText(ctx, b, msg)
	default:
		return // stickers etc — ignore
	}

	if err != nil {
		log.Printf("error: %v", err)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "❌ Помилка збереження",
		})
		return
	}
}

// --- Media group ---

func bufferMediaGroup(ctx context.Context, b *bot.Bot, msg *models.Message) {
	mediaGroupsMu.Lock()
	defer mediaGroupsMu.Unlock()

	entry, exists := mediaGroups[msg.MediaGroupID]
	if exists {
		entry.timer.Reset(mediaGroupWait)
		entry.msgs = append(entry.msgs, msg)
	} else {
		entry = &mediaGroupEntry{
			msgs:   []*models.Message{msg},
			chatID: msg.Chat.ID,
		}
		groupID := msg.MediaGroupID
		entry.timer = time.AfterFunc(mediaGroupWait, func() {
			flushMediaGroup(ctx, b, groupID)
		})
		mediaGroups[msg.MediaGroupID] = entry
	}
}

func flushMediaGroup(ctx context.Context, b *bot.Bot, groupID string) {
	mediaGroupsMu.Lock()
	entry, exists := mediaGroups[groupID]
	delete(mediaGroups, groupID)
	mediaGroupsMu.Unlock()

	if !exists || len(entry.msgs) == 0 {
		return
	}

	// Find caption from any message in the group
	var caption string
	for _, msg := range entry.msgs {
		if msg.Caption != "" {
			caption = msg.Caption
			break
		}
	}

	firstMsg := entry.msgs[0]
	title := makeTitle(firstMsg, caption, "media")
	folderName := sanitizeFolderName(title)
	postFolderID := mustGetOrCreateFolder(tgAttFolderID, folderName)

	var attachments []string
	for i, msg := range entry.msgs {
		var fileID string
		var fileName string
		var mime string

		switch {
		case msg.Photo != nil:
			photo := msg.Photo[len(msg.Photo)-1]
			fileID = photo.FileID
			fileName = fmt.Sprintf("photo_%d.jpg", i+1)
			mime = "image/jpeg"
		case msg.Video != nil:
			fileID = msg.Video.FileID
			fileName = fmt.Sprintf("video_%d.mp4", i+1)
			mime = "video/mp4"
		case msg.Document != nil:
			fileID = msg.Document.FileID
			if msg.Document.FileName != "" {
				fileName = msg.Document.FileName
			} else {
				fileName = fmt.Sprintf("doc_%d", i+1)
			}
			mime = msg.Document.MimeType
			if mime == "" {
				mime = "application/octet-stream"
			}
		default:
			continue
		}

		data, err := downloadFile(ctx, b, fileID)
		if err != nil {
			log.Printf("media group download error: %v", err)
			continue
		}

		uploadBytes(postFolderID, fileName, data, mime)
		attachments = append(attachments, folderName+"/"+fileName)
	}

	if len(attachments) == 0 {
		return
	}

	note := buildNote(firstMsg, caption, attachments)
	uploadMD(inboxFolderID, title+".md", note)

	b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: entry.chatID,
		Text:   fmt.Sprintf("📎 %d files", len(attachments)),
	})
	log.Printf("media group: %s (%d files)", title, len(attachments))
}

// --- Handlers ---

func handleText(ctx context.Context, b *bot.Bot, msg *models.Message) error {
	title := makeTitle(msg, msg.Text, "text")
	note := buildNote(msg, msg.Text, nil)
	uploadMD(inboxFolderID, title+".md", note)

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: "✅"})
	log.Printf("text: %s", title)
	return nil
}

func handlePhoto(ctx context.Context, b *bot.Bot, msg *models.Message) error {
	photo := msg.Photo[len(msg.Photo)-1]
	data, err := downloadFile(ctx, b, photo.FileID)
	if err != nil {
		return fmt.Errorf("download photo: %w", err)
	}

	title := makeTitle(msg, msg.Caption, "photo")
	folderName := sanitizeFolderName(title)
	postFolderID := mustGetOrCreateFolder(tgAttFolderID, folderName)

	fileName := "photo.jpg"
	uploadBytes(postFolderID, fileName, data, "image/jpeg")

	note := buildNote(msg, msg.Caption, []string{folderName + "/" + fileName})
	uploadMD(inboxFolderID, title+".md", note)

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: "📸"})
	log.Printf("photo: %s", title)
	return nil
}

func handleDocument(ctx context.Context, b *bot.Bot, msg *models.Message) error {
	doc := msg.Document
	data, err := downloadFile(ctx, b, doc.FileID)
	if err != nil {
		return fmt.Errorf("download doc: %w", err)
	}

	title := makeTitle(msg, msg.Caption, "doc")
	folderName := sanitizeFolderName(title)
	postFolderID := mustGetOrCreateFolder(tgAttFolderID, folderName)

	fileName := doc.FileName
	if fileName == "" {
		fileName = "file"
	}
	mime := doc.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}
	uploadBytes(postFolderID, fileName, data, mime)

	note := buildNote(msg, msg.Caption, []string{folderName + "/" + fileName})
	uploadMD(inboxFolderID, title+".md", note)

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: "📎"})
	log.Printf("doc: %s", title)
	return nil
}

func handleVoice(ctx context.Context, b *bot.Bot, msg *models.Message) error {
	data, err := downloadFile(ctx, b, msg.Voice.FileID)
	if err != nil {
		return fmt.Errorf("download voice: %w", err)
	}

	title := makeTitle(msg, "", "voice")
	folderName := sanitizeFolderName(title)
	postFolderID := mustGetOrCreateFolder(tgAttFolderID, folderName)

	fileName := "voice.ogg"
	uploadBytes(postFolderID, fileName, data, "audio/ogg")

	content := fmt.Sprintf("🎤 Голосове (%dс)", msg.Voice.Duration)
	note := buildNote(msg, content, []string{folderName + "/" + fileName})
	uploadMD(inboxFolderID, title+".md", note)

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: "🎤"})
	log.Printf("voice: %s", title)
	return nil
}

func handleVideoNote(ctx context.Context, b *bot.Bot, msg *models.Message) error {
	data, err := downloadFile(ctx, b, msg.VideoNote.FileID)
	if err != nil {
		return fmt.Errorf("download video note: %w", err)
	}

	title := makeTitle(msg, "", "videonote")
	folderName := sanitizeFolderName(title)
	postFolderID := mustGetOrCreateFolder(tgAttFolderID, folderName)

	fileName := "videonote.mp4"
	uploadBytes(postFolderID, fileName, data, "video/mp4")

	content := fmt.Sprintf("🎤 Відеоповідомлення (%dс)", msg.VideoNote.Duration)
	note := buildNote(msg, content, []string{folderName + "/" + fileName})
	uploadMD(inboxFolderID, title+".md", note)

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: "🎤"})
	log.Printf("videonote: %s", title)
	return nil
}

func handleVideo(ctx context.Context, b *bot.Bot, msg *models.Message) error {
	data, err := downloadFile(ctx, b, msg.Video.FileID)
	if err != nil {
		return fmt.Errorf("download video: %w", err)
	}

	title := makeTitle(msg, msg.Caption, "video")
	folderName := sanitizeFolderName(title)
	postFolderID := mustGetOrCreateFolder(tgAttFolderID, folderName)

	fileName := "video.mp4"
	uploadBytes(postFolderID, fileName, data, "video/mp4")

	note := buildNote(msg, msg.Caption, []string{folderName + "/" + fileName})
	uploadMD(inboxFolderID, title+".md", note)

	b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, Text: "🎬"})
	log.Printf("video: %s", title)
	return nil
}

// --- Note builder ---

func buildNote(msg *models.Message, content string, attachments []string) string {
	now := time.Now()
	var sb strings.Builder

	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("date: %s\n", now.Format("2006-01-02 15:04")))
	sb.WriteString("tags: [inbox, telegram]\n")

	fwd := forwardInfo(msg)
	if fwd != "" {
		sb.WriteString(fmt.Sprintf("forwarded_from: \"%s\"\n", fwd))
	}
	sb.WriteString("---\n\n")

	if fwd != "" {
		sb.WriteString(fmt.Sprintf("**Переслано від:** %s\n\n", fwd))
	}
	if content != "" {
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	for _, att := range attachments {
		sb.WriteString(fmt.Sprintf("![[%s]]\n\n", att))
	}

	return sb.String()
}

func forwardInfo(msg *models.Message) string {
	if msg.ForwardOrigin == nil {
		return ""
	}
	switch msg.ForwardOrigin.Type {
	case models.MessageOriginTypeUser:
		if msg.ForwardOrigin.MessageOriginUser != nil {
			u := msg.ForwardOrigin.MessageOriginUser.SenderUser
			if u.Username != "" {
				return fmt.Sprintf("%s %s (@%s)", u.FirstName, u.LastName, u.Username)
			}
			return strings.TrimSpace(u.FirstName + " " + u.LastName)
		}
	case models.MessageOriginTypeChat:
		if msg.ForwardOrigin.MessageOriginChat != nil {
			return msg.ForwardOrigin.MessageOriginChat.SenderChat.Title
		}
	case models.MessageOriginTypeChannel:
		if msg.ForwardOrigin.MessageOriginChannel != nil {
			return msg.ForwardOrigin.MessageOriginChannel.Chat.Title
		}
	case models.MessageOriginTypeHiddenUser:
		if msg.ForwardOrigin.MessageOriginHiddenUser != nil {
			return msg.ForwardOrigin.MessageOriginHiddenUser.SenderUserName
		}
	}
	return ""
}

// --- Google Drive ---

func mustGetOrCreateFolder(parentID, name string) string {
	q := fmt.Sprintf("'%s' in parents and name='%s' and mimeType='application/vnd.google-apps.folder' and trashed=false", parentID, name)
	list, err := driveService.Files.List().Q(q).Fields("files(id)").Do()
	if err != nil {
		log.Fatalf("drive list: %v", err)
	}
	if len(list.Files) > 0 {
		return list.Files[0].Id
	}

	f, err := driveService.Files.Create(&drive.File{
		Name:     name,
		MimeType: "application/vnd.google-apps.folder",
		Parents:  []string{parentID},
	}).Fields("id").Do()
	if err != nil {
		log.Fatalf("drive create folder: %v", err)
	}
	return f.Id
}

func uploadMD(folderID, name, content string) {
	uploadBytes(folderID, name, []byte(content), "text/markdown")
}

func uploadBytes(folderID, name string, data []byte, mime string) {
	_, err := driveService.Files.Create(&drive.File{
		Name:    name,
		Parents: []string{folderID},
	}).Media(strings.NewReader(string(data))).Do()
	if err != nil {
		log.Printf("drive upload error: %v", err)
	}
}

// --- Telegram file download ---

func downloadFile(ctx context.Context, b *bot.Bot, fileID string) ([]byte, error) {
	f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, err
	}

	url := b.FileDownloadLink(f)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// --- OAuth2 ---

func getOAuthClient(credsFile, tokenFile string) (*http.Client, error) {
	b, err := os.ReadFile(credsFile)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	config, err := google.ConfigFromJSON(b, drive.DriveFileScope)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}

	tok, err := loadToken(tokenFile)
	if err != nil {
		tok, err = getTokenFromWeb(config)
		if err != nil {
			return nil, err
		}
		saveToken(tokenFile, tok)
	}

	return config.Client(context.Background(), tok), nil
}

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	return tok, json.NewDecoder(f).Decode(tok)
}

func saveToken(path string, token *oauth2.Token) {
	f, err := os.Create(path)
	if err != nil {
		log.Printf("save token: %v", err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func getTokenFromWeb(config *oauth2.Config) (*oauth2.Token, error) {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Open this URL in your browser:\n%s\n\nEnter authorization code: ", authURL)

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return nil, fmt.Errorf("read auth code: %w", err)
	}

	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("exchange token: %w", err)
	}
	return tok, nil
}

// --- Helpers ---

func ts() string {
	return time.Now().Format("2006-01-02_15-04-05")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing env: %s", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
