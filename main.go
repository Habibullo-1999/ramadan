package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bot exposes a minimal Telegram client (no external deps) built on long polling.
type Bot struct {
	token         string
	apiURL        string
	client        *http.Client
	offset        int
	state         *StateStore
	calendars     map[string][]DayTimes
	tz            *time.Location
	scheduler     *ReminderManager
	hadiths       []string
	niyatSuhoor   string
	niyatIftar    string
	ramadanStart  time.Time
	defaultRegion string
}

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

type User struct {
	ID int64 `json:"id"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type sendMessageRequest struct {
	ChatID                int64       `json:"chat_id"`
	Text                  string      `json:"text"`
	ReplyMarkup           interface{} `json:"reply_markup,omitempty"`
	ParseMode             string      `json:"parse_mode,omitempty"`
	DisableWebPagePreview bool        `json:"disable_web_page_preview"`
}

// DayTimes keeps prayer times in minutes from midnight for a single Ramadan day.
type DayTimes struct {
	Data      string
	Day       int
	SuhoorEnd int // Same as Fajr by default, reminder goes 30m before.
	Fajr      int
	Dhuhr     int
	Asr       int
	Maghrib   int
	Isha      int
}

// StateStore keeps chat-specific preferences in memory.
type StateStore struct {
	mu    sync.Mutex
	users map[int64]*UserSettings
}

type UserSettings struct {
	Region         string
	Notifications  bool
	RegionSelected bool
}

// ReminderManager schedules 30-minute-before notifications for each chat.
type ReminderManager struct {
	mu           sync.Mutex
	active       map[int64]*reminderState
	calendar     map[string][]DayTimes
	loc          *time.Location
	ramadanStart time.Time
	sendFn       func(chatID int64, text string) error
	hadiths      []string
	niyatSuhoor  string
	niyatIftar   string
}

type reminderState struct {
	cancel context.CancelFunc
	region string
}

type eventSpec struct {
	Key       string
	Title     string
	Time      time.Time
	IsNiyat   bool
	UseIftar  bool
	UseSuhoor bool
}

func main() {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		log.Fatalf("TELEGRAM_BOT_TOKEN is not set")
	}
	rand.Seed(time.Now().UnixNano())
	log.Printf("Go version: %s", runtime.Version())

	loc, err := time.LoadLocation("Asia/Dushanbe")
	if err != nil {
		log.Fatalf("failed to load Asia/Dushanbe timezone: %v", err)
	}

	calendars := buildCalendars()
	hadiths := sampleHadiths()
	niyatSuhoor, niyatIftar := niyatTexts()
	start := resolveRamadanStart(loc)

	state := &StateStore{users: make(map[int64]*UserSettings)}
	bot := newBot(token, state, calendars, loc, hadiths, niyatSuhoor, niyatIftar, start)
	if err := bot.setCommands(); err != nil {
		log.Printf("setMyCommands error: %v", err)
	}

	log.Printf("Ramadan bot is running. Ramadan start: %s", start.Format("2006-01-02"))
	ctx := context.Background()
	bot.Run(ctx)
}

func newBot(token string, state *StateStore, calendars map[string][]DayTimes, tz *time.Location, hadiths []string, niyatSuhoor, niyatIftar string, start time.Time) *Bot {
	b := &Bot{
		token:         token,
		apiURL:        fmt.Sprintf("https://api.telegram.org/bot%s", token),
		client:        &http.Client{Timeout: 30 * time.Second},
		state:         state,
		calendars:     calendars,
		tz:            tz,
		hadiths:       hadiths,
		niyatSuhoor:   niyatSuhoor,
		niyatIftar:    niyatIftar,
		ramadanStart:  start,
		defaultRegion: "Душанбе",
	}

	manager := &ReminderManager{
		active:       make(map[int64]*reminderState),
		calendar:     calendars,
		loc:          tz,
		ramadanStart: start,
		hadiths:      hadiths,
		niyatSuhoor:  niyatSuhoor,
		niyatIftar:   niyatIftar,
	}
	manager.sendFn = func(chatID int64, text string) error {
		return b.SendMessage(chatID, text, nil)
	}
	b.scheduler = manager

	return b
}

// setCommands configures the Telegram bot menu (client-side command list).
func (b *Bot) setCommands() error {
	commands := []BotCommand{
		{Command: "start", Description: "Запуск бота и выбор региона"},
		{Command: "menu", Description: "Меню и клавиатура"},
		{Command: "region", Description: "Выбрать регион Таджикистана"},
		{Command: "calendar", Description: "Календарь Рамадана по регионам"},
		{Command: "today", Description: "Времена на сегодня"},
		{Command: "notifyon", Description: "Включить напоминания"},
		{Command: "notifyoff", Description: "Выключить напоминания"},
	}

	body := struct {
		Commands []BotCommand `json:"commands"`
	}{Commands: commands}

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/setMyCommands", b.apiURL), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram setMyCommands error %d: %s", result.ErrorCode, result.Description)
	}
	return nil
}

// Run starts long polling loop and dispatches updates.
func (b *Bot) Run(ctx context.Context) {
	for {
		updates, err := b.getUpdates(ctx)
		if err != nil {
			log.Printf("getUpdates error: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
			switch {
			case u.CallbackQuery != nil:
				b.handleCallback(u.CallbackQuery)
			case u.Message != nil:
				b.handleMessage(u.Message)
			}
		}
	}
}

func (b *Bot) getUpdates(ctx context.Context) ([]Update, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/getUpdates", b.apiURL), nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Set("timeout", "25")
	if b.offset > 0 {
		q.Set("offset", strconv.Itoa(b.offset))
	}
	req.URL.RawQuery = q.Encode()

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var envelope struct {
		OK          bool     `json:"ok"`
		Result      []Update `json:"result"`
		Description string   `json:"description"`
		ErrorCode   int      `json:"error_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if !envelope.OK {
		return nil, fmt.Errorf("telegram getUpdates error %d: %s", envelope.ErrorCode, envelope.Description)
	}
	return envelope.Result, nil
}

func (b *Bot) SendMessage(chatID int64, text string, markup interface{}) error {
	return b.SendMessageWithMode(chatID, text, markup, "")
}

func (b *Bot) SendMessageWithMode(chatID int64, text string, markup interface{}, parseMode string) error {
	body := sendMessageRequest{
		ChatID:                chatID,
		Text:                  text,
		ReplyMarkup:           markup,
		ParseMode:             parseMode,
		DisableWebPagePreview: true,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/sendMessage", b.apiURL), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool     `json:"ok"`
		Description string   `json:"description"`
		ErrorCode   int      `json:"error_code"`
		Result      *Message `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("telegram sendMessage error %d: %s", result.ErrorCode, result.Description)
	}
	return nil
}

func (b *Bot) answerCallback(id string) {
	data := url.Values{}
	data.Set("callback_query_id", id)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/answerCallbackQuery", b.apiURL), strings.NewReader(data.Encode()))
	if err != nil {
		log.Printf("answerCallback build error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.client.Do(req)
	if err != nil {
		log.Printf("answerCallback error: %v", err)
		return
	}
	resp.Body.Close()
}

func (b *Bot) handleMessage(msg *Message) {
	text := strings.TrimSpace(msg.Text)
	lower := strings.ToLower(text)
	switch {
	case lower == "/start":
		b.handleStart(msg.Chat.ID)
	case lower == "/menu" || lower == "/help":
		b.handleStart(msg.Chat.ID)
	case lower == "/region":
		b.promptRegion(msg.Chat.ID, "Выберите свой регион Таджикистана:")
	case lower == "/calendar":
		b.sendCalendar(msg.Chat.ID)
	case lower == "/today":
		b.sendToday(msg.Chat.ID)
	case lower == "/notifyoff":
		b.setNotifications(msg.Chat.ID, false)
	case lower == "/notifyon":
		b.setNotifications(msg.Chat.ID, true)
	default:
		b.sendHelp(msg.Chat.ID)
	}
}

func (b *Bot) handleStart(chatID int64) {
	welcome := "Ассалому алейкум! Я помогу с календарём Рамадана, напоминаниями и ниётами.\n\n" +
		"Команды:\n" +
		"/region — выбрать регион\n" +
		"/calendar — календарь (Сухур и Ифтар)\n" +
		"/today — Сухур и Ифтар на сегодня\n" +
		"/notifyoff — выключить напоминания\n" +
		"/notifyon — включить напоминания\n" +
		"/menu или /help — меню и клавиатура"
	if err := b.SendMessage(chatID, welcome, b.regionKeyboard()); err != nil {
		log.Printf("send welcome error: %v", err)
	}
}

func (b *Bot) promptRegion(chatID int64, message string) {
	if err := b.SendMessage(chatID, message, b.regionKeyboard()); err != nil {
		log.Printf("prompt region error: %v", err)
	}
}

func (b *Bot) handleCallback(cb *CallbackQuery) {
	if cb.Data == "" {
		return
	}
	b.answerCallback(cb.ID)

	if strings.HasPrefix(cb.Data, "region:") {
		region := strings.TrimPrefix(cb.Data, "region:")
		chatID := cb.From.ID
		if cb.Message != nil {
			chatID = cb.Message.Chat.ID
		}
		b.state.SetRegion(chatID, region)
		if err := b.SendMessage(chatID, fmt.Sprintf("Регион выбран: %s\nНапоминания включены автоматически (за 30 минут до каждого намаза, сухура и ифтара).", region), nil); err != nil {
			log.Printf("confirm region error: %v", err)
		}
		b.scheduler.Start(chatID, region)
		return
	}
}

func (b *Bot) sendHelp(chatID int64) {
	help := "Команды:\n" +
		"/region — выбор региона\n" +
		"/calendar — календарь (Сухур и Ифтар)\n" +
		"/today — Сухур и Ифтар на сегодня\n" +
		"/notifyoff — выключить напоминания\n" +
		"/notifyon — включить напоминания\n" +
		"/menu или /help — меню и клавиатура"
	if err := b.SendMessage(chatID, help, b.regionKeyboard()); err != nil {
		log.Printf("help send error: %v", err)
	}
}

func (b *Bot) sendCalendar(chatID int64) {
	settings := b.state.Get(chatID)
	region := settings.Region
	if region == "" {
		region = b.defaultRegion
	}
	schedule, ok := b.calendars[region]
	if !ok {
		b.SendMessage(chatID, "Сначала выберите регион /region", nil)
		return
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Календарь Рамадана (%s)\nНачало: %s\n", region, b.ramadanStart.Format("2006-01-02")))
	builder.WriteString("```\n")
	builder.WriteString("Дата       | День | Сухур | Ифтар\n")
	builder.WriteString("----       | ---- | ----- | -----\n")
	for _, day := range schedule {
		if day.Day == 0 {
			builder.WriteString(fmt.Sprintf("%s | --   | %5s | %5s\n",
				day.Data,
				minutesToClock(day.SuhoorEnd),
				minutesToClock(day.Maghrib)))
			continue
		}
		builder.WriteString(fmt.Sprintf("%s | %02d   | %5s | %5s\n",
			day.Data,
			day.Day,
			minutesToClock(day.SuhoorEnd),
			minutesToClock(day.Maghrib)))
	}
	builder.WriteString(fmt.Sprintf("```\n\n-Сухур завершается во время Фаджр. Магриб — время ифтара. \n\n\n %s", b.hadiths[rand.Intn(len(b.hadiths))]))

	if err := b.SendMessageWithMode(chatID, builder.String(), nil, "Markdown"); err != nil {
		log.Printf("calendar send error: %v", err)
	}
}

func (b *Bot) sendToday(chatID int64) {
	settings := b.state.Get(chatID)
	if settings.Region == "" {
		b.promptRegion(chatID, "Сначала выберите регион:")
		return
	}
	cal, ok := b.calendars[settings.Region]
	if !ok || len(cal) == 0 {
		b.SendMessage(chatID, "Не найден календарь для выбранного региона. Переустановите регион командой /region.", nil)
		return
	}
	//now := time.Now().In(b.tz)
	//if now.Before(b.ramadanStart) {
	//	first := cal[0]
	//	msg := fmt.Sprintf("Рамадан начинается %s.\nРегион: %s\nДень 1 — Сухур до: %s\nИфтар (Магриб): %s\nНапоминания включатся в день начала. \n\n\n%s",
	//		b.ramadanStart.Format("2006-01-02"),
	//		settings.Region,
	//		minutesToClock(first.SuhoorEnd),
	//		minutesToClock(first.Maghrib),
	//		b.hadiths[rand.Intn(len(b.hadiths))],
	//	)
	//	b.SendMessage(chatID, msg, nil)
	//	return
	//}
	day := currentDaySchedule(cal, b.ramadanStart, b.tz)
	if day == nil {
		b.SendMessage(chatID, "Сейчас вне диапазона календаря Рамадана. Проверьте дату начала в переменной RAMADAN_START.", nil)
		return
	}

	text := fmt.Sprintf("Напоминание о Рамадане\nРегион: %s\nДата:%s\nДень %d\nСухур до: %s\nИфтар (Магриб): %s",
		settings.Region,
		day.Data,
		day.Day,
		minutesToClock(day.SuhoorEnd),
		minutesToClock(day.Maghrib))
	if err := b.SendMessage(chatID, text, nil); err != nil {
		log.Printf("today send error: %v", err)
	}
}

func (b *Bot) setNotifications(chatID int64, enabled bool) {
	settings := b.state.Get(chatID)
	if settings.Region == "" {
		b.promptRegion(chatID, "Выберите регион, чтобы управлять напоминаниями:")
		return
	}
	b.state.SetNotifications(chatID, enabled)
	if enabled {
		b.scheduler.Start(chatID, settings.Region)
		b.SendMessage(chatID, "Напоминания включены.", nil)
	} else {
		b.scheduler.Stop(chatID)
		b.SendMessage(chatID, "Напоминания выключены.", nil)
	}
}

func (b *Bot) regionKeyboard() InlineKeyboardMarkup {
	regions := []string{
		"Душанбе",
		"Ашт",
		"Айни",
		"Кулоб",
		"Рашт",
		"Хамадони",
		"Худжанд",
		"Истаравшан",
		"Исфара",
		"Конибодом",
		"Хоруг",
		"Мургоб",
		"Ш. Шохин",
		"Муъминобод",
		"Панчакент",
		"Шахритус",
		"Н. Хусрав",
		"Турсунзода",
	}

	var rows [][]InlineKeyboardButton
	for _, r := range regions {
		rows = append(rows, []InlineKeyboardButton{
			{Text: r, CallbackData: "region:" + r},
		})
	}
	return InlineKeyboardMarkup{InlineKeyboard: rows}
}

// StateStore helpers.
func (s *StateStore) Get(chatID int64) *UserSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, ok := s.users[chatID]
	if !ok {
		settings = &UserSettings{}
		s.users[chatID] = settings
	}
	return settings
}

func (s *StateStore) SetRegion(chatID int64, region string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, ok := s.users[chatID]
	if !ok {
		settings = &UserSettings{}
		s.users[chatID] = settings
	}
	settings.Region = region
	settings.Notifications = true
	settings.RegionSelected = true
}

func (s *StateStore) SetNotifications(chatID int64, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, ok := s.users[chatID]
	if !ok {
		settings = &UserSettings{}
		s.users[chatID] = settings
	}
	settings.Notifications = enabled
}

// ReminderManager handles per-chat reminder goroutines.
func (rm *ReminderManager) Start(chatID int64, region string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if existing, ok := rm.active[chatID]; ok {
		existing.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	rm.active[chatID] = &reminderState{cancel: cancel, region: region}
	go rm.loop(ctx, chatID, region)
}

func (rm *ReminderManager) Stop(chatID int64) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if existing, ok := rm.active[chatID]; ok {
		existing.cancel()
		delete(rm.active, chatID)
	}
}

func (rm *ReminderManager) loop(ctx context.Context, chatID int64, region string) {
	calendar, ok := rm.calendar[region]
	if !ok {
		rm.sendFn(chatID, fmt.Sprintf("Не найден календарь для региона %s", region))
		return
	}

	loc := rm.loc
	for {
		now := time.Now().In(loc)
		if now.Before(rm.ramadanStart) {
			wait := time.Until(rm.ramadanStart)
			rm.sendFn(chatID, fmt.Sprintf("До начала Рамадана осталось %.0f часов. Напоминания включатся автоматически.", wait.Hours()))
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		day := currentDaySchedule(calendar, rm.ramadanStart, loc)
		if day == nil {
			// Out of range: Rely on start date to tell user.
			rm.sendFn(chatID, "Календарь Рамадана завершён или старт ещё не наступил. Проверьте дату RAMADAN_START.")
			time.Sleep(6 * time.Hour)
			continue
		}

		baseDate := rm.ramadanStart.AddDate(0, 0, day.Day-1)
		base := time.Date(baseDate.Year(), baseDate.Month(), baseDate.Day(), 0, 0, 0, 0, loc)
		events := []eventSpec{
			{Key: "suhoor", Title: "Сухур (конец приёма)", Time: base.Add(time.Duration(day.SuhoorEnd) * time.Minute), UseSuhoor: true},
			{Key: "fajr", Title: "Фаджр", Time: base.Add(time.Duration(day.Fajr) * time.Minute)},
			{Key: "dhuhr", Title: "Зухр", Time: base.Add(time.Duration(day.Dhuhr) * time.Minute)},
			{Key: "asr", Title: "Аср", Time: base.Add(time.Duration(day.Asr) * time.Minute)},
			{Key: "maghrib", Title: "Магриб (Ифтар)", Time: base.Add(time.Duration(day.Maghrib) * time.Minute), UseIftar: true},
			{Key: "isha", Title: "Иша", Time: base.Add(time.Duration(day.Isha) * time.Minute)},
		}

		sent := make(map[string]bool)
		nextDay := base.Add(24 * time.Hour)
		ticker := time.NewTicker(30 * time.Second)

	loopDay:
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				now = time.Now().In(loc)
				if !now.Before(nextDay) {
					break loopDay
				}
				for _, ev := range events {
					remindAt := ev.Time.Add(-30 * time.Minute)
					if now.After(remindAt) && !sent[ev.Key] {
						sent[ev.Key] = true
						rm.sendReminder(chatID, region, day.Day, ev)
					}
				}
			}
		}
	}
}

func (rm *ReminderManager) sendReminder(chatID int64, region string, day int, ev eventSpec) {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Регион: %s\nДень %d Рамадана\nЧерез 30 минут: %s в %s\n",
		region,
		day,
		ev.Title,
		ev.Time.In(rm.loc).Format("15:04")))

	if ev.UseSuhoor {
		builder.WriteString("\nНият сухур:\n")
		builder.WriteString(rm.niyatSuhoor)
	} else if ev.UseIftar {
		builder.WriteString("\nНият ифтар:\n")
		builder.WriteString(rm.niyatIftar)
	} else {
		builder.WriteString("\nХадис дня:\n")
		builder.WriteString(rm.randomHadith())
	}

	if err := rm.sendFn(chatID, builder.String()); err != nil {
		log.Printf("reminder send error: %v", err)
	}
}

func (rm *ReminderManager) randomHadith() string {
	if len(rm.hadiths) == 0 {
		return ""
	}
	return rm.hadiths[rand.Intn(len(rm.hadiths))]
}

func minutesToClock(min int) string {
	h := min / 60
	m := min % 60
	return fmt.Sprintf("%02d:%02d", h, m)
}

func cleanClock(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		if r == '-' {
			break
		}
		if (r >= '0' && r <= '9') || r == ':' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func mustClockToMinutes(raw string) int {
	clean := cleanClock(raw)
	t, err := time.Parse("15:04", clean)
	if err != nil {
		log.Fatalf("cannot parse time %q (cleaned %q): %v", raw, clean, err)
	}
	return t.Hour()*60 + t.Minute()
}

// currentDaySchedule returns the DayTimes for today's Ramadan day relative to start.
func currentDaySchedule(days []DayTimes, start time.Time, loc *time.Location) *DayTimes {
	now := time.Now().In(loc)

	dayIndex := int(now.Sub(start).Hours()/24) + 1
	if dayIndex < 0 || dayIndex > len(days) {
		return nil
	}
	for i := range days {
		if days[i].Day == dayIndex {
			return &days[i]
		}
	}
	return nil
}

func applyOffset(day DayTimes, offset int) DayTimes {
	adjust := func(val int) int {
		out := val + offset
		if out < 0 {
			return 0
		}
		return out
	}
	return DayTimes{
		Data:      day.Data,
		Day:       day.Day,
		SuhoorEnd: adjust(day.SuhoorEnd),
		Fajr:      adjust(day.Fajr),
		Dhuhr:     adjust(day.Dhuhr),
		Asr:       adjust(day.Asr),
		Maghrib:   adjust(day.Maghrib),
		Isha:      adjust(day.Isha),
	}
}

func resolveRamadanStart(loc *time.Location) time.Time {
	env := strings.TrimSpace(os.Getenv("RAMADAN_START"))
	if env != "" {
		if parsed, err := time.ParseInLocation("2006-01-02", env, loc); err == nil {
			return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, loc)
		}
		log.Printf("Could not parse RAMADAN_START (%s), fallback to Feb 19 logic", env)
	}
	now := time.Now().In(loc)
	year := now.Year()
	feb19 := time.Date(year, time.February, 19, 0, 0, 0, 0, loc)
	if now.After(feb19) {
		feb19 = time.Date(year+1, time.February, 19, 0, 0, 0, 0, loc)
	}
	return feb19
}

// buildCalendars loads 30-дневный календарь (19.02–20.03.2026) для Душанбе и применяет смещения по регионам.
func buildCalendars() map[string][]DayTimes {
	base := []struct {
		Date    string
		Day     int
		Fajr    string
		Dhuhr   string
		Asr     string
		Maghrib string
		Isha    string
	}{
		{"18.02.2026", 0, "05:42", "12:41", "15:40", "18:13", "19:29"},
		{"19.02.2026", 1, "05:41", "12:41", "15:40", "18:14", "19:30"},
		{"20.02.2026", 2, "05:40", "12:40", "15:41", "18:15", "19:31"},
		{"21.02.2026", 3, "05:39", "12:39", "15:41", "18:16", "19:32"},
		{"22.02.2026", 4, "05:38", "12:38", "15:42", "18:17", "19:33"},
		{"23.02.2026", 5, "05:37", "12:38", "15:43", "18:18", "19:34"},
		{"24.02.2026", 6, "05:35", "12:38", "15:44", "18:20", "19:35"},
		{"25.02.2026", 7, "05:34", "12:38", "15:44", "18:21", "19:36"},
		{"26.02.2026", 8, "05:32", "12:38", "15:45", "18:22", "19:37"},
		{"27.02.2026", 9, "05:31", "12:38", "15:46", "18:23", "19:38"},
		{"28.02.2026", 10, "05:29", "12:37", "15:47", "18:24", "19:39"},
		{"01.03.2026", 11, "05:28", "12:37", "15:48", "18:26", "19:41"},
		{"02.03.2026", 12, "05:27", "12:37", "15:48", "18:27", "19:42"},
		{"03.03.2026", 13, "05:26", "12:37", "15:49", "18:28", "19:43"},
		{"04.03.2026", 14, "05:24", "12:37", "15:50", "18:29", "19:44"},
		{"05.03.2026", 15, "05:22", "12:36", "15:50", "18:30", "19:45"},
		{"06.03.2026", 16, "05:20", "12:36", "15:51", "18:31", "19:46"},
		{"07.03.2026", 17, "05:19", "12:36", "15:51", "18:32", "19:47"},
		{"08.03.2026", 18, "05:17", "12:36", "15:52", "18:33", "19:48"},
		{"09.03.2026", 19, "05:16", "12:35", "15:53", "18:34", "19:49"},
		{"10.03.2026", 20, "05:14", "12:35", "15:53", "18:35", "19:50"},
		{"11.03.2026", 21, "05:13", "12:35", "15:54", "18:36", "19:51"},
		{"12.03.2026", 22, "05:11", "12:38", "15:55", "18:37", "19:52"},
		{"13.03.2026", 23, "05:10", "12:38", "15:55", "18:38", "19:53"},
		{"14.03.2026", 24, "05:08", "12:38", "15:56", "18:39", "19:54"},
		{"15.03.2026", 25, "05:07", "12:38", "15:56", "18:40", "19:55"},
		{"16.03.2026", 26, "05:05", "12:38", "15:57", "18:41", "19:56"},
		{"17.03.2026", 27, "05:04", "12:37", "15:57", "18:42", "19:57"},
		{"18.03.2026", 28, "05:02", "12:36", "15:57", "18:43", "19:58"},
		{"19.03.2026", 29, "05:01", "12:35", "15:58", "18:44", "19:59"},
		{"20.03.2026", 30, "05:00", "12:34", "15:58", "18:45", "20:00"},
	}

	var baseDays []DayTimes
	for _, d := range base {
		fajr := mustClockToMinutes(d.Fajr)
		dhuhr := mustClockToMinutes(d.Dhuhr)
		asr := mustClockToMinutes(d.Asr)
		maghrib := mustClockToMinutes(d.Maghrib)
		isha := mustClockToMinutes(d.Isha)
		baseDays = append(baseDays, DayTimes{
			Data:      d.Date,
			Day:       d.Day,
			SuhoorEnd: fajr,
			Fajr:      fajr,
			Dhuhr:     dhuhr,
			Asr:       asr,
			Maghrib:   maghrib,
			Isha:      isha,
		})
	}

	offsets := map[string]int{
		"Душанбе":    0,
		"Ашт":        -6,
		"Айни":       1,
		"Кулоб":      -4,
		"Рашт":       -6,
		"Хамадони":   -3,
		"Худжанд":    -3,
		"Истаравшан": -1,
		"Исфара":     -7,
		"Конибодом":  -6,
		"Хоруг":      -11,
		"Мургоб":     -20,
		"Ш. Шохин":   -5,
		"Муъминобод": -3,
		"Панчакент":  5,
		"Шахритус":   3,
		"Н. Хусрав":  4,
		"Турсунзода": 3,
	}

	calendars := make(map[string][]DayTimes)
	for region, offset := range offsets {
		days := make([]DayTimes, len(baseDays))
		for i, bd := range baseDays {
			days[i] = applyOffset(bd, offset)
		}
		calendars[region] = days
	}

	return calendars
}

func sampleHadiths() []string {
	return []string{
		"\"Руза сипар аст\" — ҳадис от Пророка ﷺ (Бухари).",
		"\"Тот, кто уверовал и будет поститься в Рамадан ради Аллаха, тому простятся прошлые грехи\" — хадис от Абу Хурайры (Бухари, Муслим).",
		"\"У постящегося две радости: при разговении и когда встретит своего Господа\" — хадис от Абу Хурайры (Бухари).",
		"\"Дуа постящегося при разговении не отвергается\" — хадис (Тирмизи).",
		"\"Пять намазов, джума к джуме и Рамадан к Рамадану искупают (грехи), если избегаются большие грехи\" — хадис (Муслим).",
		"\"Пост — это щит, и если кто-то из вас постится, пусть не говорит непристойностей и не кричит\" — хадис от Абу Хурайры (Бухари).",
		"\"Когда наступает Рамадан, врата Рая открываются, врата Ада закрываются, а шайтанов сковывают\" — хадис от Абу Хурайры (Бухари, Муслим).",
	}
}

func niyatTexts() (string, string) {
	niyatSuhoor := `Нияти Рӯзаи мохи шарифи Рамазон
Ба забони Арабӣ:
Валисавми ғаддин мин шаҳри рамазоналлазӣ фаризатан навайту.
Бо забони Тоҷикӣ:
Ният кардам рӯзаи моҳи шарифи Рамазон аз субҳи содиқ то фурӯ рафтани офтоб.`
	niyatIftar := `Дуъои Ифтор (Кушодани руза):
Ба забони Арабӣ:
Аллоҳума лака сумту ва бика оманту ва алайка таваккалту ва ало ризқиқа афтарту. Бираҳматика ё арҳамар роҳимин.
Бо забони Тоҷикӣ:
Парвардигоро! Барои ризогии Ту рӯза доштам ва ба Ту имон овардам ва ба Ту такя дорам ва ба ризқи додаи Ту ифтор кардам.`
	return niyatSuhoor, niyatIftar
}
