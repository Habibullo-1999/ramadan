package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
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
	hadithsByLang map[string][]string
	niyatSuhoor   map[string]string
	niyatIftar    map[string]string
	ramadanStart  time.Time
	defaultRegion string
	imageCache    *imageCache
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

type ReplyKeyboardMarkup struct {
	Keyboard              [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard        bool               `json:"resize_keyboard,omitempty"`
	IsPersistent          bool               `json:"is_persistent,omitempty"`
	InputFieldPlaceholder string             `json:"input_field_placeholder,omitempty"`
}

type KeyboardButton struct {
	Text string `json:"text"`
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
	mu          sync.Mutex
	users       map[int64]*UserSettings
	persistPath string
}

type UserSettings struct {
	Language       string
	Region         string
	Notifications  bool
	RegionSelected bool
}

// ReminderManager schedules 30-minute-before notifications for each chat.
type ReminderManager struct {
	mu            sync.Mutex
	active        map[int64]*reminderState
	calendar      map[string][]DayTimes
	loc           *time.Location
	ramadanStart  time.Time
	sendFn        func(chatID int64, text string) error
	sendPhotoFn   func(chatID int64, photo []byte, caption string) error
	getLangFn     func(chatID int64) string
	hadithsByLang map[string][]string
	niyatSuhoor   map[string]string
	niyatIftar    map[string]string
	imageCache    *imageCache
}

type imageCache struct {
	mu    sync.RWMutex
	items map[string]cachedImage
}

type cachedImage struct {
	data      []byte
	expiresAt time.Time
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

const (
	langTG = "tg"
	langRU = "ru"
	langEN = "en"
	langUZ = "uz"
)

var translations = map[string]map[string]string{
	langTG: {
		"choose_language":         "Лутфан забони худро интихоб кунед:\n\nТоҷикӣ / Русский / English / O'zbek",
		"language_saved":          "Забон интихоб шуд.",
		"choose_region":           "Минтақаи худро интихоб кунед:",
		"welcome":                 "Ассалому алайкум! Ман барои тақвими Рамазон, ёдовариҳо ва ниятҳо кӯмак мекунам.\n\nФармонҳо:\n/lang — ивази забон\n/region — интихоби минтақа\n/calendar — тақвими Рамазон (саҳар ва ифтор)\n/today — вақтҳои имрӯз (саҳар ва ифтор)\n/notifyoff — хомӯш кардани ёдовариҳо\n/notifyon — фаъол кардани ёдовариҳо\n/testnotify — ирсоли ёдоварии санҷишӣ\n/menu ё /help — меню ва клавиатура",
		"help":                    "Фармонҳо:\n/lang — ивази забон\n/region — интихоби минтақа\n/calendar — тақвими Рамазон (саҳар ва ифтор)\n/today — вақтҳои имрӯз (саҳар ва ифтор)\n/notifyoff — хомӯш кардани ёдовариҳо\n/notifyon — фаъол кардани ёдовариҳо\n/testnotify — ирсоли ёдоварии санҷишӣ\n/menu ё /help — меню ва клавиатура",
		"region_selected":         "Минтақа интихоб шуд: %s\nЁдовариҳо ба таври худкор фаъол шуданд (30 дақиқа пеш аз ҳар намоз, саҳар ва ифтор).",
		"need_region_first":       "Лутфан аввал минтақаро бо /region интихоб кунед.",
		"calendar_not_found":      "Тақвим барои минтақаи интихобшуда ёфт нашуд. Минтақаро бо /region аз нав интихоб кунед.",
		"out_of_range":            "Ҳоло берун аз доираи тақвими Рамазон аст. Санаи оғозро дар RAMADAN_START санҷед.",
		"calendar_caption":        "Тақвими Рамазон (%s)\n\n%s",
		"today_caption":           "%s • %s • Рӯзи %d\n\n%s",
		"test_region_default":     "Минтақа интихоб нашудааст, санҷиш барои минтақаи %s фиристода мешавад.",
		"test_notification_title": "Ёдоварии санҷишӣ",
		"need_region_notify":      "Барои идоракунии ёдовариҳо минтақаро интихоб кунед:",
		"notify_enabled":          "Ёдовариҳо фаъол шуданд.",
		"notify_disabled":         "Ёдовариҳо хомӯш шуданд.",
		"rem_no_calendar_region":  "Тақвим барои минтақаи %s ёфт нашуд.",
		"rem_before_start":        "То оғози Рамазон %.0f соат монд. Ёдовариҳо худкор фаъол мешаванд.",
		"rem_out_of_range":        "Тақвими Рамазон анҷом ёфтааст ё ҳанӯз оғоз нашудааст. Лутфан RAMADAN_START-ро санҷед.",
		"rem_headline":            "Минтақа: %s\nРӯзи %d Рамазон\nБаъд аз 30 дақиқа: %s соати %s",
		"niyat_suhoor_label":      "Нияти саҳар:\n",
		"niyat_iftar_label":       "Нияти ифтор:\n",
		"hadith_day_title":        "Ҳадиси рӯз",
		"hadith_title_default":    "Ҳадис",
		"hadith_source":           "Манбаъ",
		"hadith_fallback":         "Аллоҳ рӯза ва ибодатҳои шуморо қабул фармояд.",
		"img_calendar_title":      "Тақвими моҳи шарифи Рамазон",
		"img_start_prefix":        "Оғоз ",
		"img_calendar_subtitle":   "Вақти саҳар ва ифтор",
		"img_30_days":             "30 рӯз",
		"img_col_date":            "Сана",
		"img_col_day":             "Рӯз",
		"img_col_suhoor":          "Саҳар",
		"img_col_iftar":           "Ифтор",
		"img_today_marker":        "Им",
		"img_calendar_footer":     "«Рӯза сипар аст» — ҳадис аз Паёмбар ﷺ (Бухорӣ).",
		"img_today_title":         "Имрӯз дар Рамазон",
		"img_region_prefix":       "Минтақа: ",
		"img_date_day":            "Сана: %s    Рӯз: %d",
		"img_today_suhoor_label":  "Саҳар то",
		"img_today_iftar_label":   "Ифтор",
		"img_today_footer":        "Саҳар бо даромадани намози бомдод анҷом мешавад.",
		"img_rem_title":           "Ёдоварии намоз",
		"img_rem_day_date":        "Рӯзи %d • %s",
		"img_rem_footer":          "Баъд аз 30 дақиқа. Пешакӣ омода шавед.",
		"event_suhoor":            "Саҳар (охири вақт)",
		"event_fajr":              "Бомдод",
		"event_dhuhr":             "Пешин",
		"event_asr":               "Аср",
		"event_maghrib":           "Шом (ифтор)",
		"event_isha":              "Хуфтан",
		"btn_calendar":            "🗓 Тақвим",
		"btn_today":               "🌙 Имрӯз",
		"btn_region":              "📍 Минтақа",
		"btn_lang":                "🌐 Забон",
		"btn_notify_on":           "🔔 Ёдоварӣ ON",
		"btn_notify_off":          "🔕 Ёдоварӣ OFF",
		"btn_help":                "ℹ️ Ёрӣ",
	},
	langRU: {
		"choose_language":         "Выберите язык:\n\nТоҷикӣ / Русский / English / O'zbek",
		"language_saved":          "Язык выбран.",
		"choose_region":           "Выберите свой регион:",
		"welcome":                 "Ассалому алейкум! Я помогу с календарём Рамадана, напоминаниями и ниётами.\n\nКоманды:\n/lang — сменить язык\n/region — выбрать регион\n/calendar — календарь Рамадана (сухур и ифтар)\n/today — времена на сегодня (сухур и ифтар)\n/notifyoff — выключить напоминания\n/notifyon — включить напоминания\n/testnotify — отправить тест уведомления\n/menu или /help — меню и клавиатура",
		"help":                    "Команды:\n/lang — сменить язык\n/region — выбор региона\n/calendar — календарь Рамадана (сухур и ифтар)\n/today — времена на сегодня (сухур и ифтар)\n/notifyoff — выключить напоминания\n/notifyon — включить напоминания\n/testnotify — отправить тест уведомления\n/menu или /help — меню и клавиатура",
		"region_selected":         "Регион выбран: %s\nНапоминания включены автоматически (за 30 минут до каждого намаза, сухура и ифтара).",
		"need_region_first":       "Сначала выберите регион через /region.",
		"calendar_not_found":      "Календарь для выбранного региона не найден. Переустановите регион командой /region.",
		"out_of_range":            "Сейчас вне диапазона календаря Рамадана. Проверьте дату RAMADAN_START.",
		"calendar_caption":        "Календарь Рамадана (%s)\n\n%s",
		"today_caption":           "%s • %s • День %d\n\n%s",
		"test_region_default":     "Регион не выбран, тест отправляется для региона: %s",
		"test_notification_title": "Тестовое уведомление",
		"need_region_notify":      "Выберите регион для управления напоминаниями:",
		"notify_enabled":          "Напоминания включены.",
		"notify_disabled":         "Напоминания выключены.",
		"rem_no_calendar_region":  "Не найден календарь для региона %s.",
		"rem_before_start":        "До начала Рамадана осталось %.0f часов. Напоминания включатся автоматически.",
		"rem_out_of_range":        "Календарь Рамадана завершён или ещё не начался. Проверьте RAMADAN_START.",
		"rem_headline":            "Регион: %s\nДень %d Рамадана\nЧерез 30 минут: %s в %s",
		"niyat_suhoor_label":      "Ният сухур:\n",
		"niyat_iftar_label":       "Ният ифтар:\n",
		"hadith_day_title":        "Хадис дня",
		"hadith_title_default":    "Хадис",
		"hadith_source":           "Источник",
		"hadith_fallback":         "Пусть Аллах примет ваш пост и молитвы.",
		"img_calendar_title":      "Календарь Рамадана",
		"img_start_prefix":        "Старт ",
		"img_calendar_subtitle":   "Время сухура и ифтара",
		"img_30_days":             "30 дней",
		"img_col_date":            "Дата",
		"img_col_day":             "День",
		"img_col_suhoor":          "Сухур",
		"img_col_iftar":           "Ифтар",
		"img_today_marker":        "Сег",
		"img_calendar_footer":     "«Пост — это щит» — хадис Пророка ﷺ (Бухари).",
		"img_today_title":         "Сегодня в Рамадан",
		"img_region_prefix":       "Регион: ",
		"img_date_day":            "Дата: %s    День: %d",
		"img_today_suhoor_label":  "Сухур до",
		"img_today_iftar_label":   "Ифтар",
		"img_today_footer":        "Сухур завершается с наступлением Фаджра.",
		"img_rem_title":           "Напоминание о намазе",
		"img_rem_day_date":        "День %d • %s",
		"img_rem_footer":          "Через 30 минут. Подготовьтесь заранее.",
		"event_suhoor":            "Сухур (конец времени)",
		"event_fajr":              "Фаджр",
		"event_dhuhr":             "Зухр",
		"event_asr":               "Аср",
		"event_maghrib":           "Магриб (ифтар)",
		"event_isha":              "Иша",
		"btn_calendar":            "🗓 Календарь",
		"btn_today":               "🌙 Сегодня",
		"btn_region":              "📍 Регион",
		"btn_lang":                "🌐 Язык",
		"btn_notify_on":           "🔔 Напоминания ON",
		"btn_notify_off":          "🔕 Напоминания OFF",
		"btn_help":                "ℹ️ Помощь",
	},
	langEN: {
		"choose_language":         "Choose language:\n\nТоҷикӣ / Русский / English / O'zbek",
		"language_saved":          "Language selected.",
		"choose_region":           "Select your region:",
		"welcome":                 "Assalamu alaikum! I can help with Ramadan calendar, reminders, and niyat texts.\n\nCommands:\n/lang — change language\n/region — select region\n/calendar — Ramadan calendar (suhoor and iftar)\n/today — today timings (suhoor and iftar)\n/notifyoff — disable reminders\n/notifyon — enable reminders\n/testnotify — send test reminder\n/menu or /help — menu and keyboard",
		"help":                    "Commands:\n/lang — change language\n/region — select region\n/calendar — Ramadan calendar (suhoor and iftar)\n/today — today timings (suhoor and iftar)\n/notifyoff — disable reminders\n/notifyon — enable reminders\n/testnotify — send test reminder\n/menu or /help — menu and keyboard",
		"region_selected":         "Region selected: %s\nReminders enabled automatically (30 minutes before each prayer, suhoor and iftar).",
		"need_region_first":       "Please select a region first with /region.",
		"calendar_not_found":      "Calendar for selected region not found. Re-select region with /region.",
		"out_of_range":            "Current date is outside Ramadan calendar range. Check RAMADAN_START.",
		"calendar_caption":        "Ramadan Calendar (%s)\n\n%s",
		"today_caption":           "%s • %s • Day %d\n\n%s",
		"test_region_default":     "Region is not selected, test is sent for region: %s",
		"test_notification_title": "Test reminder",
		"need_region_notify":      "Select region to manage reminders:",
		"notify_enabled":          "Reminders enabled.",
		"notify_disabled":         "Reminders disabled.",
		"rem_no_calendar_region":  "Calendar for region %s not found.",
		"rem_before_start":        "Ramadan starts in %.0f hours. Reminders will start automatically.",
		"rem_out_of_range":        "Ramadan calendar ended or has not started yet. Check RAMADAN_START.",
		"rem_headline":            "Region: %s\nRamadan day %d\nIn 30 minutes: %s at %s",
		"niyat_suhoor_label":      "Suhoor niyat:\n",
		"niyat_iftar_label":       "Iftar niyat:\n",
		"hadith_day_title":        "Hadith of the day",
		"hadith_title_default":    "Hadith",
		"hadith_source":           "Source",
		"hadith_fallback":         "May Allah accept your fasting and prayers.",
		"img_calendar_title":      "Ramadan Calendar",
		"img_start_prefix":        "Start ",
		"img_calendar_subtitle":   "Suhoor and iftar times",
		"img_30_days":             "30 days",
		"img_col_date":            "Date",
		"img_col_day":             "Day",
		"img_col_suhoor":          "Suhoor",
		"img_col_iftar":           "Iftar",
		"img_today_marker":        "Now",
		"img_calendar_footer":     "\"Fasting is a shield\" — Hadith of the Prophet ﷺ (Bukhari).",
		"img_today_title":         "Today in Ramadan",
		"img_region_prefix":       "Region: ",
		"img_date_day":            "Date: %s    Day: %d",
		"img_today_suhoor_label":  "Suhoor until",
		"img_today_iftar_label":   "Iftar",
		"img_today_footer":        "Suhoor ends with the time of Fajr.",
		"img_rem_title":           "Prayer reminder",
		"img_rem_day_date":        "Day %d • %s",
		"img_rem_footer":          "In 30 minutes. Prepare in advance.",
		"event_suhoor":            "Suhoor (end time)",
		"event_fajr":              "Fajr",
		"event_dhuhr":             "Dhuhr",
		"event_asr":               "Asr",
		"event_maghrib":           "Maghrib (iftar)",
		"event_isha":              "Isha",
		"btn_calendar":            "🗓 Calendar",
		"btn_today":               "🌙 Today",
		"btn_region":              "📍 Region",
		"btn_lang":                "🌐 Language",
		"btn_notify_on":           "🔔 Reminders ON",
		"btn_notify_off":          "🔕 Reminders OFF",
		"btn_help":                "ℹ️ Help",
	},
	langUZ: {
		"choose_language":         "Tilni tanlang:\n\nТоҷикӣ / Русский / English / O'zbek",
		"language_saved":          "Til tanlandi.",
		"choose_region":           "Mintaqangizni tanlang:",
		"welcome":                 "Assalomu alaykum! Men Ramazon taqvimi, eslatmalar va niyatlarda yordam beraman.\n\nBuyruqlar:\n/lang — tilni almashtirish\n/region — mintaqani tanlash\n/calendar — Ramazon taqvimi (saharlik va iftor)\n/today — bugungi vaqtlar (saharlik va iftor)\n/notifyoff — eslatmalarni o‘chirish\n/notifyon — eslatmalarni yoqish\n/testnotify — test eslatma yuborish\n/menu yoki /help — menyu va klaviatura",
		"help":                    "Buyruqlar:\n/lang — tilni almashtirish\n/region — mintaqani tanlash\n/calendar — Ramazon taqvimi (saharlik va iftor)\n/today — bugungi vaqtlar (saharlik va iftor)\n/notifyoff — eslatmalarni o‘chirish\n/notifyon — eslatmalarni yoqish\n/testnotify — test eslatma yuborish\n/menu yoki /help — menyu va klaviatura",
		"region_selected":         "Mintaqa tanlandi: %s\nEslatmalar avtomatik yoqildi (har namoz, saharlik va iftordan 30 daqiqa oldin).",
		"need_region_first":       "Avval /region orqali mintaqani tanlang.",
		"calendar_not_found":      "Tanlangan mintaqa uchun taqvim topilmadi. /region bilan qayta tanlang.",
		"out_of_range":            "Hozir sana Ramazon taqvimi oralig‘idan tashqarida. RAMADAN_START ni tekshiring.",
		"calendar_caption":        "Ramazon taqvimi (%s)\n\n%s",
		"today_caption":           "%s • %s • Kun %d\n\n%s",
		"test_region_default":     "Mintaqa tanlanmagan, test ushbu mintaqa uchun yuboriladi: %s",
		"test_notification_title": "Test eslatma",
		"need_region_notify":      "Eslatmalarni boshqarish uchun mintaqani tanlang:",
		"notify_enabled":          "Eslatmalar yoqildi.",
		"notify_disabled":         "Eslatmalar o‘chirildi.",
		"rem_no_calendar_region":  "%s mintaqasi uchun taqvim topilmadi.",
		"rem_before_start":        "Ramazon boshlanishiga %.0f soat qoldi. Eslatmalar avtomatik yoqiladi.",
		"rem_out_of_range":        "Ramazon taqvimi tugagan yoki hali boshlanmagan. RAMADAN_START ni tekshiring.",
		"rem_headline":            "Mintaqa: %s\nRamazon kuni %d\n30 daqiqadan so‘ng: %s soat %s da",
		"niyat_suhoor_label":      "Saharlik niyati:\n",
		"niyat_iftar_label":       "Iftor niyati:\n",
		"hadith_day_title":        "Kun hadisi",
		"hadith_title_default":    "Hadis",
		"hadith_source":           "Manba",
		"hadith_fallback":         "Alloh ro‘za va ibodatlaringizni qabul qilsin.",
		"img_calendar_title":      "Ramazon taqvimi",
		"img_start_prefix":        "Boshlanish ",
		"img_calendar_subtitle":   "Saharlik va iftor vaqtlari",
		"img_30_days":             "30 kun",
		"img_col_date":            "Sana",
		"img_col_day":             "Kun",
		"img_col_suhoor":          "Saharlik",
		"img_col_iftar":           "Iftor",
		"img_today_marker":        "Bug",
		"img_calendar_footer":     "\"Ro‘za qalqondir\" — Payg‘ambar ﷺ hadisi (Buxoriy).",
		"img_today_title":         "Bugun Ramazonda",
		"img_region_prefix":       "Mintaqa: ",
		"img_date_day":            "Sana: %s    Kun: %d",
		"img_today_suhoor_label":  "Saharlik gacha",
		"img_today_iftar_label":   "Iftor",
		"img_today_footer":        "Saharlik Fajr kirishi bilan tugaydi.",
		"img_rem_title":           "Namoz eslatmasi",
		"img_rem_day_date":        "Kun %d • %s",
		"img_rem_footer":          "30 daqiqadan so‘ng. Oldindan tayyor bo‘ling.",
		"event_suhoor":            "Saharlik (yakun vaqti)",
		"event_fajr":              "Bomdod",
		"event_dhuhr":             "Peshin",
		"event_asr":               "Asr",
		"event_maghrib":           "Shom (iftor)",
		"event_isha":              "Xufton",
		"btn_calendar":            "🗓 Taqvim",
		"btn_today":               "🌙 Bugun",
		"btn_region":              "📍 Mintaqa",
		"btn_lang":                "🌐 Til",
		"btn_notify_on":           "🔔 Eslatma ON",
		"btn_notify_off":          "🔕 Eslatma OFF",
		"btn_help":                "ℹ️ Yordam",
	},
}

func normalizeLang(raw string) string {
	lang := strings.ToLower(strings.TrimSpace(raw))
	lang = strings.ReplaceAll(lang, "_", "-")
	if idx := strings.Index(lang, "-"); idx > 0 {
		lang = lang[:idx]
	}
	switch lang {
	case langTG, "tj", "tjk", "tajik", "taj":
		return langTG
	case langRU, "rus", "russian":
		return langRU
	case langEN, "eng", "english":
		return langEN
	case langUZ, "uzb", "ozbek", "o'zbek":
		return langUZ
	default:
		return ""
	}
}

func tr(lang, key string) string {
	lang = normalizeLang(lang)
	if lang == "" {
		lang = langTG
	}
	if dict, ok := translations[lang]; ok {
		if text, ok := dict[key]; ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	if dict, ok := translations[langTG]; ok {
		if text, ok := dict[key]; ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return key
}

func trf(lang, key string, args ...any) string {
	return fmt.Sprintf(tr(lang, key), args...)
}

func eventTitle(lang string, ev eventSpec) string {
	if title := strings.TrimSpace(ev.Title); title != "" {
		return title
	}
	return tr(lang, "event_"+ev.Key)
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
	hadiths := sampleHadithsByLang()
	niyatSuhoor, niyatIftar := niyatTextsByLang()
	start := resolveRamadanStart(loc)

	statePath := strings.TrimSpace(os.Getenv("STATE_FILE"))
	if statePath == "" {
		statePath = "state.json"
	}

	state, err := newStateStore(statePath)
	if err != nil {
		log.Fatalf("failed to initialize state store: %v", err)
	}
	bot := newBot(token, state, calendars, loc, hadiths, niyatSuhoor, niyatIftar, start)
	if err := bot.setCommands(); err != nil {
		log.Printf("setMyCommands error: %v", err)
	}
	restored := state.ActiveNotificationRegions()
	for chatID, region := range restored {
		bot.scheduler.Start(chatID, region)
	}
	log.Printf("Restored %d notification subscriptions from %s", len(restored), statePath)

	log.Printf("Ramadan bot is running. Ramadan start: %s", start.Format("2006-01-02"))
	ctx := context.Background()
	bot.Run(ctx)
}

func newBot(token string, state *StateStore, calendars map[string][]DayTimes, tz *time.Location, hadiths map[string][]string, niyatSuhoor, niyatIftar map[string]string, start time.Time) *Bot {
	cache := newImageCache()
	b := &Bot{
		token:         token,
		apiURL:        fmt.Sprintf("https://api.telegram.org/bot%s", token),
		client:        &http.Client{Timeout: 30 * time.Second},
		state:         state,
		calendars:     calendars,
		tz:            tz,
		hadithsByLang: hadiths,
		niyatSuhoor:   niyatSuhoor,
		niyatIftar:    niyatIftar,
		ramadanStart:  start,
		defaultRegion: "Душанбе",
		imageCache:    cache,
	}

	manager := &ReminderManager{
		active:        make(map[int64]*reminderState),
		calendar:      calendars,
		loc:           tz,
		ramadanStart:  start,
		hadithsByLang: hadiths,
		niyatSuhoor:   niyatSuhoor,
		niyatIftar:    niyatIftar,
		imageCache:    cache,
	}
	manager.sendFn = func(chatID int64, text string) error {
		return b.SendMessage(chatID, text, nil)
	}
	manager.sendPhotoFn = func(chatID int64, photo []byte, caption string) error {
		return b.SendPhoto(chatID, photo, caption)
	}
	manager.getLangFn = func(chatID int64) string {
		return b.userLang(chatID)
	}
	b.scheduler = manager

	return b
}

// setCommands configures the Telegram bot menu (client-side command list).
func (b *Bot) setCommands() error {
	commands := []BotCommand{
		{Command: "start", Description: "Start / Язык / Til"},
		{Command: "lang", Description: "Change language"},
		{Command: "menu", Description: "Menu / Help"},
		{Command: "region", Description: "Region / Регион / Минтақа"},
		{Command: "calendar", Description: "Ramadan calendar"},
		{Command: "today", Description: "Today timings"},
		{Command: "notifyon", Description: "Enable reminders"},
		{Command: "notifyoff", Description: "Disable reminders"},
		{Command: "testnotify", Description: "Test reminder"},
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

func (b *Bot) SendPhoto(chatID int64, photo []byte, caption string) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	if caption != "" {
		if err := writer.WriteField("caption", caption); err != nil {
			return err
		}
	}

	part, err := writer.CreateFormFile("photo", "calendar.png")
	if err != nil {
		return err
	}
	if _, err := part.Write(photo); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/sendPhoto", b.apiURL), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

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
		return fmt.Errorf("telegram sendPhoto error %d: %s", result.ErrorCode, result.Description)
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

func normalizeButtonText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
}

func (b *Bot) resolveCommand(chatID int64, text string) string {
	normalized := normalizeButtonText(text)
	if normalized == "" {
		return ""
	}
	switch normalized {
	case "/start", "/menu", "/help", "/lang", "/language", "/region", "/calendar", "/today", "/notifyon", "/notifyoff", "/testnotify":
		return normalized
	}

	buttonToCommand := map[string]string{
		"btn_calendar":   "/calendar",
		"btn_today":      "/today",
		"btn_region":     "/region",
		"btn_lang":       "/lang",
		"btn_notify_on":  "/notifyon",
		"btn_notify_off": "/notifyoff",
		"btn_help":       "/help",
	}
	checkLangs := []string{langTG, langRU, langEN, langUZ, b.userLang(chatID)}
	for _, l := range checkLangs {
		for key, command := range buttonToCommand {
			if normalized == normalizeButtonText(tr(l, key)) {
				return command
			}
		}
	}

	return normalized
}

func (b *Bot) handleMessage(msg *Message) {
	lower := b.resolveCommand(msg.Chat.ID, msg.Text)
	switch {
	case lower == "/start":
		b.handleStart(msg.Chat.ID)
	case lower == "/lang" || lower == "/language":
		b.promptLanguage(msg.Chat.ID)
	case lower == "/menu":
		b.handleStart(msg.Chat.ID)
	case lower == "/help":
		if _, ok := b.requireLanguage(msg.Chat.ID); !ok {
			return
		}
		b.sendHelp(msg.Chat.ID)
	case lower == "/region":
		if lang, ok := b.requireLanguage(msg.Chat.ID); ok {
			b.promptRegion(msg.Chat.ID, tr(lang, "choose_region"))
		}
	case lower == "/calendar":
		if _, ok := b.requireLanguage(msg.Chat.ID); ok {
			b.sendCalendar(msg.Chat.ID)
		}
	case lower == "/today":
		if _, ok := b.requireLanguage(msg.Chat.ID); ok {
			b.sendToday(msg.Chat.ID)
		}
	case lower == "/notifyoff":
		if _, ok := b.requireLanguage(msg.Chat.ID); ok {
			b.setNotifications(msg.Chat.ID, false)
		}
	case lower == "/notifyon":
		if _, ok := b.requireLanguage(msg.Chat.ID); ok {
			b.setNotifications(msg.Chat.ID, true)
		}
	case lower == "/testnotify":
		if _, ok := b.requireLanguage(msg.Chat.ID); ok {
			b.sendTestNotification(msg.Chat.ID)
		}
	default:
		if _, ok := b.requireLanguage(msg.Chat.ID); ok {
			b.sendHelp(msg.Chat.ID)
		}
	}
}

func (b *Bot) userLang(chatID int64) string {
	lang := normalizeLang(b.state.Get(chatID).Language)
	if lang == "" {
		return langTG
	}
	return lang
}

func (b *Bot) requireLanguage(chatID int64) (string, bool) {
	settings := b.state.Get(chatID)
	lang := normalizeLang(settings.Language)
	if lang == "" {
		b.promptLanguage(chatID)
		return "", false
	}
	if lang != settings.Language {
		b.state.SetLanguage(chatID, lang)
	}
	return lang, true
}

func (b *Bot) promptLanguage(chatID int64) {
	if err := b.SendMessage(chatID, tr(b.userLang(chatID), "choose_language"), b.languageKeyboard()); err != nil {
		log.Printf("prompt language error: %v", err)
	}
}

func (b *Bot) handleStart(chatID int64) {
	settings := b.state.Get(chatID)
	lang := normalizeLang(settings.Language)
	if lang == "" {
		b.promptLanguage(chatID)
		return
	}
	if err := b.SendMessage(chatID, tr(lang, "welcome"), b.menuKeyboard(lang)); err != nil {
		log.Printf("send welcome error: %v", err)
	}
	if strings.TrimSpace(settings.Region) == "" {
		b.promptRegion(chatID, tr(lang, "choose_region"))
		return
	}
}

func (b *Bot) promptRegion(chatID int64, message string) {
	if strings.TrimSpace(message) == "" {
		message = tr(b.userLang(chatID), "choose_region")
	}
	if err := b.SendMessage(chatID, message, b.regionKeyboard()); err != nil {
		log.Printf("prompt region error: %v", err)
	}
}

func (b *Bot) handleCallback(cb *CallbackQuery) {
	if cb.Data == "" {
		return
	}
	b.answerCallback(cb.ID)

	chatID := cb.From.ID
	if cb.Message != nil {
		chatID = cb.Message.Chat.ID
	}
	if strings.HasPrefix(cb.Data, "lang:") {
		lang := normalizeLang(strings.TrimPrefix(cb.Data, "lang:"))
		if lang == "" {
			lang = langTG
		}
		b.state.SetLanguage(chatID, lang)
		if err := b.SendMessage(chatID, tr(lang, "language_saved"), nil); err != nil {
			log.Printf("confirm language error: %v", err)
		}
		if strings.TrimSpace(b.state.Get(chatID).Region) == "" {
			b.promptRegion(chatID, tr(lang, "choose_region"))
		} else {
			b.sendHelp(chatID)
		}
		return
	}

	if strings.HasPrefix(cb.Data, "region:") {
		lang, ok := b.requireLanguage(chatID)
		if !ok {
			return
		}
		region := strings.TrimPrefix(cb.Data, "region:")
		b.state.SetRegion(chatID, region)
		if err := b.SendMessage(chatID, trf(lang, "region_selected", region), nil); err != nil {
			log.Printf("confirm region error: %v", err)
		}
		b.scheduler.Start(chatID, region)
		return
	}
}

func (b *Bot) sendHelp(chatID int64) {
	lang := b.userLang(chatID)
	if err := b.SendMessage(chatID, tr(lang, "help"), b.menuKeyboard(lang)); err != nil {
		log.Printf("help send error: %v", err)
	}
}

func (b *Bot) sendCalendar(chatID int64) {
	settings := b.state.Get(chatID)
	lang := b.userLang(chatID)
	region := settings.Region
	if region == "" {
		region = b.defaultRegion
	}
	schedule, ok := b.calendars[region]
	if !ok {
		b.SendMessage(chatID, tr(lang, "need_region_first"), nil)
		return
	}

	photo, err := b.cachedCalendarImage(lang, region, schedule)
	if err != nil {
		log.Printf("calendar image build error: %v", err)
	} else {
		caption := trf(
			lang,
			"calendar_caption",
			region,
			formatHadithBlock(lang, tr(lang, "hadith_day_title"), b.randomHadith(lang)),
		)
		if err := b.SendPhoto(chatID, photo, caption); err != nil {
			log.Printf("calendar photo send error: %v", err)
		}
	}

	//if err := b.SendMessage(chatID, text, nil); err != nil {
	//	log.Printf("calendar text send error: %v", err)
	//}
}

func (b *Bot) sendToday(chatID int64) {
	settings := b.state.Get(chatID)
	lang := b.userLang(chatID)
	if settings.Region == "" {
		b.promptRegion(chatID, tr(lang, "need_region_first"))
		return
	}
	cal, ok := b.calendars[settings.Region]
	if !ok || len(cal) == 0 {
		b.SendMessage(chatID, tr(lang, "calendar_not_found"), nil)
		return
	}
	day := currentDaySchedule(cal, b.ramadanStart, b.tz)
	if day == nil {
		b.SendMessage(chatID, tr(lang, "out_of_range"), nil)
		return
	}

	photo, err := b.cachedTodayImage(lang, settings.Region, *day)
	if err != nil {
		log.Printf("today image build error: %v", err)
	} else {
		caption := trf(
			lang,
			"today_caption",
			settings.Region,
			day.Data,
			day.Day,
			formatHadithBlock(lang, tr(lang, "hadith_day_title"), b.randomHadith(lang)),
		)
		if err := b.SendPhoto(chatID, photo, caption); err != nil {
			log.Printf("today photo send error: %v", err)
		}
	}
}

func (b *Bot) sendTestNotification(chatID int64) {
	settings := b.state.Get(chatID)
	lang := b.userLang(chatID)
	region := strings.TrimSpace(settings.Region)
	if region == "" {
		region = b.defaultRegion
		if err := b.SendMessage(chatID, trf(lang, "test_region_default", region), nil); err != nil {
			log.Printf("test notify region info send error: %v", err)
		}
	}

	dayNumber := 1
	if schedule, ok := b.calendars[region]; ok {
		if day := currentDaySchedule(schedule, b.ramadanStart, b.tz); day != nil && day.Day > 0 {
			dayNumber = day.Day
		}
	}

	ev := eventSpec{
		Key:   "test",
		Title: tr(lang, "test_notification_title"),
		Time:  time.Now().In(b.tz).Add(30 * time.Minute),
	}
	b.scheduler.sendReminder(chatID, region, dayNumber, ev)
}

func (b *Bot) setNotifications(chatID int64, enabled bool) {
	settings := b.state.Get(chatID)
	lang := b.userLang(chatID)
	if settings.Region == "" {
		b.promptRegion(chatID, tr(lang, "need_region_notify"))
		return
	}
	b.state.SetNotifications(chatID, enabled)
	if enabled {
		b.scheduler.Start(chatID, settings.Region)
		b.SendMessage(chatID, tr(lang, "notify_enabled"), nil)
	} else {
		b.scheduler.Stop(chatID)
		b.SendMessage(chatID, tr(lang, "notify_disabled"), nil)
	}
}

func (b *Bot) menuKeyboard(lang string) ReplyKeyboardMarkup {
	return ReplyKeyboardMarkup{
		Keyboard: [][]KeyboardButton{
			{
				{Text: tr(lang, "btn_calendar")},
				{Text: tr(lang, "btn_today")},
			},
			{
				{Text: tr(lang, "btn_region")},
				{Text: tr(lang, "btn_lang")},
			},
			{
				{Text: tr(lang, "btn_notify_on")},
				{Text: tr(lang, "btn_notify_off")},
			},
			{
				{Text: tr(lang, "btn_help")},
			},
		},
		ResizeKeyboard: true,
		IsPersistent:   true,
	}
}

func (b *Bot) languageKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Тоҷикӣ", CallbackData: "lang:" + langTG},
				{Text: "Русский", CallbackData: "lang:" + langRU},
			},
			{
				{Text: "English", CallbackData: "lang:" + langEN},
				{Text: "O'zbek", CallbackData: "lang:" + langUZ},
			},
		},
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
type persistedStateData struct {
	Users map[string]UserSettings `json:"users"`
}

func newStateStore(path string) (*StateStore, error) {
	store := &StateStore{
		users:       make(map[int64]*UserSettings),
		persistPath: strings.TrimSpace(path),
	}
	if err := store.loadFromDisk(); err != nil {
		return nil, err
	}
	return store, nil
}

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
	settings, ok := s.users[chatID]
	if !ok {
		settings = &UserSettings{}
		s.users[chatID] = settings
	}
	settings.Region = region
	settings.Notifications = true
	settings.RegionSelected = true
	snapshot := s.snapshotLocked()
	path := s.persistPath
	s.mu.Unlock()

	if err := writeStateSnapshot(path, snapshot); err != nil {
		log.Printf("state persist error (SetRegion): %v", err)
	}
}

func (s *StateStore) SetLanguage(chatID int64, lang string) {
	s.mu.Lock()
	settings, ok := s.users[chatID]
	if !ok {
		settings = &UserSettings{}
		s.users[chatID] = settings
	}
	settings.Language = normalizeLang(lang)
	snapshot := s.snapshotLocked()
	path := s.persistPath
	s.mu.Unlock()

	if err := writeStateSnapshot(path, snapshot); err != nil {
		log.Printf("state persist error (SetLanguage): %v", err)
	}
}

func (s *StateStore) SetNotifications(chatID int64, enabled bool) {
	s.mu.Lock()
	settings, ok := s.users[chatID]
	if !ok {
		settings = &UserSettings{}
		s.users[chatID] = settings
	}
	settings.Notifications = enabled
	snapshot := s.snapshotLocked()
	path := s.persistPath
	s.mu.Unlock()

	if err := writeStateSnapshot(path, snapshot); err != nil {
		log.Printf("state persist error (SetNotifications): %v", err)
	}
}

func (s *StateStore) ActiveNotificationRegions() map[int64]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make(map[int64]string)
	for chatID, settings := range s.users {
		if settings == nil {
			continue
		}
		region := strings.TrimSpace(settings.Region)
		if !settings.Notifications || region == "" {
			continue
		}
		result[chatID] = region
	}
	return result
}

func (s *StateStore) loadFromDisk() error {
	path := strings.TrimSpace(s.persistPath)
	if path == "" {
		return nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var data persistedStateData
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for key, settings := range data.Users {
		chatID, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			log.Printf("skip invalid chat id in persisted state: %q", key)
			continue
		}
		copySettings := settings
		s.users[chatID] = &copySettings
	}
	return nil
}

func (s *StateStore) snapshotLocked() map[string]UserSettings {
	out := make(map[string]UserSettings, len(s.users))
	for chatID, settings := range s.users {
		if settings == nil {
			continue
		}
		out[strconv.FormatInt(chatID, 10)] = *settings
	}
	return out
}

func writeStateSnapshot(path string, snapshot map[string]UserSettings) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}

	data := persistedStateData{Users: snapshot}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

func reminderDayBaseTime(ramadanStart time.Time, ramadanDay int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	baseDate := ramadanStart.AddDate(0, 0, ramadanDay-1)
	return time.Date(baseDate.Year(), baseDate.Month(), baseDate.Day(), 0, 0, 0, 0, loc)
}

func reminderEventsForDay(base time.Time, day DayTimes) []eventSpec {
	return []eventSpec{
		{Key: "suhoor", Time: base.Add(time.Duration(day.SuhoorEnd) * time.Minute), UseSuhoor: true},
		{Key: "fajr", Time: base.Add(time.Duration(day.Fajr) * time.Minute)},
		{Key: "dhuhr", Time: base.Add(time.Duration(day.Dhuhr) * time.Minute)},
		{Key: "asr", Time: base.Add(time.Duration(day.Asr) * time.Minute)},
		{Key: "maghrib", Time: base.Add(time.Duration(day.Maghrib) * time.Minute), UseIftar: true},
		{Key: "isha", Time: base.Add(time.Duration(day.Isha) * time.Minute)},
	}
}

func shouldTriggerReminder(now time.Time, ev eventSpec, sent map[string]bool) bool {
	if sent != nil && sent[ev.Key] {
		return false
	}
	remindAt := ev.Time.Add(-30 * time.Minute)
	return !now.Before(remindAt)
}

func (rm *ReminderManager) loop(ctx context.Context, chatID int64, region string) {
	lang := langTG
	if rm.getLangFn != nil {
		if resolved := normalizeLang(rm.getLangFn(chatID)); resolved != "" {
			lang = resolved
		}
	}
	calendar, ok := rm.calendar[region]
	if !ok {
		rm.sendFn(chatID, trf(lang, "rem_no_calendar_region", region))
		return
	}

	loc := rm.loc
	for {
		if rm.getLangFn != nil {
			if resolved := normalizeLang(rm.getLangFn(chatID)); resolved != "" {
				lang = resolved
			}
		}
		now := time.Now().In(loc)
		if now.Before(rm.ramadanStart) {
			wait := time.Until(rm.ramadanStart)
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
			rm.sendFn(chatID, tr(lang, "rem_out_of_range"))
			time.Sleep(6 * time.Hour)
			continue
		}

		base := reminderDayBaseTime(rm.ramadanStart, day.Day, loc)
		events := reminderEventsForDay(base, *day)

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
					if shouldTriggerReminder(now, ev, sent) {
						sent[ev.Key] = true
						rm.sendReminder(chatID, region, day.Day, ev)
					}
				}
			}
		}
	}
}

func (rm *ReminderManager) sendReminder(chatID int64, region string, day int, ev eventSpec) {
	lang := langTG
	if rm.getLangFn != nil {
		if resolved := normalizeLang(rm.getLangFn(chatID)); resolved != "" {
			lang = resolved
		}
	}
	title := eventTitle(lang, ev)
	timeLabel := ev.Time.In(rm.loc).Format("15:04")
	headline := trf(lang, "rem_headline", region, day, title, timeLabel)
	photoSent := false
	if rm.sendPhotoFn != nil {
		photo, err := rm.cachedReminderImage(lang, region, day, ev)
		if err != nil {
			log.Printf("reminder image build error: %v", err)
		} else {
			if err := rm.sendPhotoFn(chatID, photo, headline); err != nil {
				log.Printf("reminder photo send error: %v", err)
			} else {
				photoSent = true
			}
		}
	}

	var builder strings.Builder
	if !photoSent {
		builder.WriteString(headline)
		builder.WriteString("\n\n")
	}
	if ev.UseSuhoor {
		builder.WriteString(tr(lang, "niyat_suhoor_label"))
		builder.WriteString(localizedNiyatText(rm.niyatSuhoor, lang))
	} else if ev.UseIftar {
		builder.WriteString(tr(lang, "niyat_iftar_label"))
		builder.WriteString(localizedNiyatText(rm.niyatIftar, lang))
	} else {
		builder.WriteString(formatHadithBlock(lang, tr(lang, "hadith_day_title"), rm.randomHadith(lang)))
	}

	if err := rm.sendFn(chatID, builder.String()); err != nil {
		log.Printf("reminder send error: %v", err)
	}
}

func (rm *ReminderManager) randomHadith(lang string) string {
	return randomHadithForLang(rm.hadithsByLang, lang)
}

func (b *Bot) randomHadith(lang string) string {
	return randomHadithForLang(b.hadithsByLang, lang)
}

func randomHadithForLang(hadithsByLang map[string][]string, lang string) string {
	if len(hadithsByLang) == 0 {
		return ""
	}
	lang = normalizeLang(lang)
	if lang == "" {
		lang = langTG
	}
	list := hadithsByLang[lang]
	if len(list) == 0 {
		list = hadithsByLang[langTG]
	}
	if len(list) == 0 {
		for _, items := range hadithsByLang {
			if len(items) > 0 {
				list = items
				break
			}
		}
	}
	if len(list) == 0 {
		return ""
	}
	return list[rand.Intn(len(list))]
}

func localizedNiyatText(niyatByLang map[string]string, lang string) string {
	if len(niyatByLang) == 0 {
		return ""
	}
	lang = normalizeLang(lang)
	if lang == "" {
		lang = langTG
	}
	text := strings.TrimSpace(niyatByLang[lang])
	if text == "" {
		text = strings.TrimSpace(niyatByLang[langTG])
	}
	if text != "" {
		return text
	}
	for _, value := range niyatByLang {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func formatHadithBlock(lang, title, hadith string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = tr(lang, "hadith_title_default")
	}
	text := strings.TrimSpace(hadith)
	if text == "" {
		text = tr(lang, "hadith_fallback")
	}

	quote := text
	source := ""
	if idx := strings.LastIndex(text, "—"); idx > 0 {
		quote = strings.TrimSpace(text[:idx])
		source = strings.TrimSpace(text[idx+len("—"):])
	}

	var b strings.Builder
	b.WriteString("╔══")
	b.WriteString(strings.Repeat("═", 2))
	b.WriteString(title)
	b.WriteString(strings.Repeat("═", 2))
	b.WriteString("══╗\n")
	b.WriteString(quote)
	if source != "" {
		b.WriteString("\n\n")
		b.WriteString(tr(lang, "hadith_source"))
		b.WriteString(": ")
		b.WriteString(source)
	}
	b.WriteString("\n╚")
	b.WriteString(strings.Repeat("═", 10))
	b.WriteString("╝")
	return b.String()
}

func newImageCache() *imageCache {
	return &imageCache{items: make(map[string]cachedImage)}
}

func (c *imageCache) getOrBuild(key string, ttl time.Duration, build func() ([]byte, error)) ([]byte, error) {
	if c == nil || ttl <= 0 {
		return build()
	}

	now := time.Now()
	c.mu.RLock()
	if cached, ok := c.items[key]; ok && now.Before(cached.expiresAt) {
		out := append([]byte(nil), cached.data...)
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	data, err := build()
	if err != nil {
		return nil, err
	}
	copied := append([]byte(nil), data...)

	c.mu.Lock()
	c.items[key] = cachedImage{
		data:      copied,
		expiresAt: time.Now().Add(ttl),
	}
	if len(c.items) > 512 {
		now = time.Now()
		for k, v := range c.items {
			if now.After(v.expiresAt) {
				delete(c.items, k)
			}
		}
	}
	c.mu.Unlock()

	return copied, nil
}

func (b *Bot) cachedCalendarImage(lang, region string, schedule []DayTimes) ([]byte, error) {
	key := calendarImageCacheKey(lang, region, b.ramadanStart, schedule)
	return b.imageCache.getOrBuild(key, 12*time.Hour, func() ([]byte, error) {
		return renderCalendarImage(schedule, b.ramadanStart, lang)
	})
}

func (b *Bot) cachedTodayImage(lang, region string, day DayTimes) ([]byte, error) {
	key := todayImageCacheKey(lang, region, day)
	ttl := timeUntilNextDay(b.tz)
	return b.imageCache.getOrBuild(key, ttl, func() ([]byte, error) {
		return renderTodayImage(region, day, lang)
	})
}

func (rm *ReminderManager) cachedReminderImage(lang, region string, day int, ev eventSpec) ([]byte, error) {
	key := reminderImageCacheKey(lang, region, day, ev)
	ttl := 2 * time.Hour
	if !ev.Time.IsZero() {
		until := time.Until(ev.Time.Add(90 * time.Minute))
		if until > 0 {
			ttl = until
		} else {
			ttl = 30 * time.Minute
		}
	}
	if ttl < 15*time.Minute {
		ttl = 15 * time.Minute
	}
	return rm.imageCache.getOrBuild(key, ttl, func() ([]byte, error) {
		return renderReminderImage(region, day, ev, rm.loc, lang)
	})
}

func calendarImageCacheKey(lang, region string, start time.Time, schedule []DayTimes) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "calendar|%s|%s|%s|%d|", lang, region, start.Format("2006-01-02"), len(schedule))
	for _, d := range schedule {
		_, _ = fmt.Fprintf(h, "%s|%d|%d|%d|%d|%d|%d|%d;", d.Data, d.Day, d.SuhoorEnd, d.Fajr, d.Dhuhr, d.Asr, d.Maghrib, d.Isha)
	}
	return fmt.Sprintf("calendar:%016x", h.Sum64())
}

func todayImageCacheKey(lang, region string, day DayTimes) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "today|%s|%s|%s|%d|%d|%d|%d|%d|%d|%d", lang, region, day.Data, day.Day, day.SuhoorEnd, day.Fajr, day.Dhuhr, day.Asr, day.Maghrib, day.Isha)
	return fmt.Sprintf("today:%016x", h.Sum64())
}

func reminderImageCacheKey(lang, region string, day int, ev eventSpec) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "reminder|%s|%s|%d|%s|%s|%s|%t|%t", lang, region, day, ev.Key, ev.Title, ev.Time.Format(time.RFC3339), ev.UseIftar, ev.UseSuhoor)
	return fmt.Sprintf("reminder:%016x", h.Sum64())
}

func timeUntilNextDay(loc *time.Location) time.Duration {
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
	ttl := time.Until(next.Add(5 * time.Minute))
	if ttl <= 0 {
		return 1 * time.Hour
	}
	return ttl
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

func renderCalendarImage(schedule []DayTimes, start time.Time, lang string) ([]byte, error) {
	if len(schedule) == 0 {
		return nil, fmt.Errorf("empty schedule")
	}
	lang = normalizeLang(lang)
	if lang == "" {
		lang = langTG
	}

	schedule = schedule[1:]

	faces, err := loadCalendarCardFaces()
	if err != nil {
		return nil, err
	}
	defer faces.Close()

	const (
		imgW         = 980
		imgMargin    = 32
		cardRadius   = 24
		headerAreaH  = 152
		tableHeaderH = 52
		rowH         = 34
		footerH      = 48
	)

	tableH := tableHeaderH + len(schedule)*rowH
	cardH := headerAreaH + tableH + footerH + 60
	imgH := cardH + imgMargin*2

	img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	drawVerticalGradient(img, color.RGBA{R: 8, G: 17, B: 33, A: 255}, color.RGBA{R: 4, G: 10, B: 22, A: 255})
	drawRadialGlow(img, imgW-190, 120, 250, color.RGBA{R: 69, G: 197, B: 173, A: 100})
	drawRadialGlow(img, 160, imgH-170, 280, color.RGBA{R: 216, G: 168, B: 79, A: 78})

	card := image.Rect(imgMargin, imgMargin, imgW-imgMargin, imgMargin+cardH)
	shadow := image.Rect(card.Min.X+6, card.Min.Y+8, card.Max.X+6, card.Max.Y+8)
	fillRoundedRect(img, shadow, cardRadius, color.RGBA{R: 2, G: 6, B: 15, A: 120})
	fillRoundedRect(img, card, cardRadius, color.RGBA{R: 96, G: 124, B: 164, A: 255})

	inner := image.Rect(card.Min.X+2, card.Min.Y+2, card.Max.X-2, card.Max.Y-2)
	fillRoundedRect(img, inner, cardRadius-2, color.RGBA{R: 13, G: 25, B: 42, A: 255})

	headerRect := image.Rect(inner.Min.X+16, inner.Min.Y+16, inner.Max.X-16, inner.Min.Y+16+headerAreaH)
	fillRoundedRect(img, headerRect, 18, color.RGBA{R: 23, G: 43, B: 70, A: 255})
	fillRoundedRect(
		img,
		image.Rect(headerRect.Min.X+1, headerRect.Min.Y+1, headerRect.Max.X-1, headerRect.Min.Y+headerRect.Dy()/2),
		16,
		color.RGBA{R: 31, G: 58, B: 94, A: 255},
	)

	titleColor := color.RGBA{R: 243, G: 247, B: 252, A: 255}
	subtitleColor := color.RGBA{R: 177, G: 194, B: 214, A: 255}
	drawTextTop(img, faces.Title, headerRect.Min.X+22, headerRect.Min.Y+18, tr(lang, "img_calendar_title"), titleColor)
	drawTextTop(img, faces.Subtitle, headerRect.Min.X+22, headerRect.Min.Y+66, tr(lang, "img_start_prefix")+start.Format("2006-01-02"), subtitleColor)
	drawTextTop(img, faces.Subtitle, headerRect.Min.X+22, headerRect.Min.Y+94, tr(lang, "img_calendar_subtitle"), subtitleColor)

	badgeText := tr(lang, "img_30_days")
	badgeW := measureTextWidth(faces.Badge, badgeText) + 28
	badgeH := 38
	badge := image.Rect(headerRect.Max.X-badgeW-18, headerRect.Min.Y+20, headerRect.Max.X-18, headerRect.Min.Y+20+badgeH)
	fillRoundedRect(img, badge, 12, color.RGBA{R: 230, G: 184, B: 102, A: 255})
	badgeTextX := badge.Min.X + (badge.Dx()-measureTextWidth(faces.Badge, badgeText))/2
	drawTextTop(img, faces.Badge, badgeTextX, badge.Min.Y+8, badgeText, color.RGBA{R: 32, G: 25, B: 15, A: 255})

	tableRect := image.Rect(inner.Min.X+18, headerRect.Max.Y+14, inner.Max.X-18, headerRect.Max.Y+14+tableH)
	fillRoundedRect(img, tableRect, 16, color.RGBA{R: 84, G: 109, B: 145, A: 255})
	tableInner := image.Rect(tableRect.Min.X+2, tableRect.Min.Y+2, tableRect.Max.X-2, tableRect.Max.Y-2)
	fillRoundedRect(img, tableInner, 14, color.RGBA{R: 12, G: 30, B: 49, A: 255})

	headerRow := image.Rect(tableInner.Min.X, tableInner.Min.Y, tableInner.Max.X, tableInner.Min.Y+tableHeaderH)
	fillRect(img, headerRow, color.RGBA{R: 24, G: 53, B: 85, A: 255})

	colDayW := 92
	colDateW := int(float64(tableInner.Dx()-colDayW) * 0.42)
	colSuhoorW := (tableInner.Dx() - colDateW - colDayW) / 2
	colIftarW := tableInner.Dx() - colDateW - colDayW - colSuhoorW

	x0 := tableInner.Min.X
	x1 := x0 + colDateW
	x2 := x1 + colDayW
	x3 := x2 + colSuhoorW
	x4 := x3 + colIftarW
	_ = x4
	padX := 14
	headerTextY := headerRow.Min.Y + (tableHeaderH-faceLineHeight(faces.TableHeader))/2
	drawTextTop(img, faces.TableHeader, x0+padX, headerTextY, tr(lang, "img_col_date"), titleColor)
	drawTextTop(img, faces.TableHeader, x1+padX, headerTextY, tr(lang, "img_col_day"), titleColor)
	drawTextTop(img, faces.TableHeader, x2+padX, headerTextY, tr(lang, "img_col_suhoor"), titleColor)
	drawTextTop(img, faces.TableHeader, x3+padX, headerTextY, tr(lang, "img_col_iftar"), titleColor)

	now := time.Now().In(start.Location())
	todayDay := int(now.Sub(start).Hours()/24) + 1
	if todayDay < 1 || todayDay > len(schedule) {
		todayDay = -1
	}

	rowTextColor := color.RGBA{R: 232, G: 240, B: 249, A: 255}
	rowA := color.RGBA{R: 18, G: 37, B: 61, A: 255}
	rowB := color.RGBA{R: 14, G: 31, B: 52, A: 255}
	preStartRow := color.RGBA{R: 31, G: 57, B: 86, A: 255}
	todayRow := color.RGBA{R: 58, G: 84, B: 120, A: 255}

	rowsTop := headerRow.Max.Y
	for i, day := range schedule {
		y0 := rowsTop + i*rowH
		y1 := y0 + rowH
		bg := rowA
		if i%2 == 1 {
			bg = rowB
		}
		if day.Day == 0 {
			bg = preStartRow
		}
		if day.Day == todayDay {
			bg = todayRow
		}
		fillRect(img, image.Rect(tableInner.Min.X, y0, tableInner.Max.X, y1), bg)

		dayLabel := fmt.Sprintf("%02d", day.Day)
		if day.Day == 0 {
			dayLabel = "--"
		}
		if day.Day == todayDay {
			dayLabel = tr(lang, "img_today_marker")
		}

		textY := y0 + (rowH-faceLineHeight(faces.TableRow))/2
		drawTextTop(img, faces.TableRow, x0+padX, textY, day.Data, rowTextColor)
		drawTextTop(img, faces.TableRow, x1+padX, textY, dayLabel, rowTextColor)
		drawTextTop(img, faces.TableRow, x2+padX, textY, minutesToClock(day.SuhoorEnd), rowTextColor)
		drawTextTop(img, faces.TableRow, x3+padX, textY, minutesToClock(day.Maghrib), rowTextColor)
	}

	grid := color.RGBA{R: 74, G: 100, B: 132, A: 255}
	fillRect(img, image.Rect(x1, tableInner.Min.Y, x1+1, tableInner.Max.Y), grid)
	fillRect(img, image.Rect(x2, tableInner.Min.Y, x2+1, tableInner.Max.Y), grid)
	fillRect(img, image.Rect(x3, tableInner.Min.Y, x3+1, tableInner.Max.Y), grid)
	for i := 0; i <= len(schedule); i++ {
		y := rowsTop + i*rowH
		fillRect(img, image.Rect(tableInner.Min.X, y, tableInner.Max.X, y+1), grid)
	}

	footerY := tableRect.Max.Y + 16
	drawTextTop(img, faces.Footer, tableRect.Min.X, footerY, tr(lang, "img_calendar_footer"), subtitleColor)

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func renderTodayImage(region string, day DayTimes, lang string) ([]byte, error) {
	lang = normalizeLang(lang)
	if lang == "" {
		lang = langTG
	}
	faces, err := loadTodayCardFaces()
	if err != nil {
		return nil, err
	}
	defer faces.Close()

	const (
		imgW       = 980
		imgH       = 650
		margin     = 34
		cardRadius = 24
	)

	img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	drawVerticalGradient(img, color.RGBA{R: 9, G: 20, B: 36, A: 255}, color.RGBA{R: 6, G: 13, B: 25, A: 255})
	drawRadialGlow(img, imgW-170, 120, 230, color.RGBA{R: 85, G: 189, B: 173, A: 95})
	drawRadialGlow(img, 180, imgH-120, 240, color.RGBA{R: 224, G: 177, B: 93, A: 68})

	card := image.Rect(margin, margin, imgW-margin, imgH-margin)
	shadow := image.Rect(card.Min.X+7, card.Min.Y+9, card.Max.X+7, card.Max.Y+9)
	fillRoundedRect(img, shadow, cardRadius, color.RGBA{R: 2, G: 6, B: 16, A: 120})
	fillRoundedRect(img, card, cardRadius, color.RGBA{R: 95, G: 123, B: 161, A: 255})

	inner := image.Rect(card.Min.X+2, card.Min.Y+2, card.Max.X-2, card.Max.Y-2)
	fillRoundedRect(img, inner, cardRadius-2, color.RGBA{R: 13, G: 25, B: 42, A: 255})

	header := image.Rect(inner.Min.X+18, inner.Min.Y+18, inner.Max.X-18, inner.Min.Y+170)
	fillRoundedRect(img, header, 18, color.RGBA{R: 25, G: 47, B: 74, A: 255})
	fillRoundedRect(
		img,
		image.Rect(header.Min.X+1, header.Min.Y+1, header.Max.X-1, header.Min.Y+header.Dy()/2),
		16,
		color.RGBA{R: 34, G: 63, B: 98, A: 255},
	)

	titleColor := color.RGBA{R: 243, G: 247, B: 252, A: 255}
	subtitleColor := color.RGBA{R: 176, G: 194, B: 215, A: 255}

	drawTextTop(img, faces.Title, header.Min.X+22, header.Min.Y+20, tr(lang, "img_today_title"), titleColor)
	drawTextTop(img, faces.Subtitle, header.Min.X+22, header.Min.Y+70, tr(lang, "img_region_prefix")+region, subtitleColor)
	drawTextTop(
		img,
		faces.Subtitle,
		header.Min.X+22,
		header.Min.Y+102,
		trf(lang, "img_date_day", day.Data, day.Day),
		subtitleColor,
	)

	progressLabel := fmt.Sprintf("%d/30", day.Day)
	progressW := 130
	progressH := 40
	progress := image.Rect(header.Max.X-progressW-22, header.Min.Y+24, header.Max.X-22, header.Min.Y+24+progressH)
	fillRoundedRect(img, progress, 12, color.RGBA{R: 230, G: 184, B: 101, A: 255})
	progressTextX := progress.Min.X + (progressW-measureTextWidth(faces.Badge, progressLabel))/2
	drawTextTop(img, faces.Badge, progressTextX, progress.Min.Y+9, progressLabel, color.RGBA{R: 33, G: 26, B: 16, A: 255})

	boxGap := 18
	boxTop := header.Max.Y + 18
	boxBottom := boxTop + 194
	boxW := (inner.Dx() - 18*2 - boxGap) / 2
	leftBox := image.Rect(inner.Min.X+18, boxTop, inner.Min.X+18+boxW, boxBottom)
	rightBox := image.Rect(leftBox.Max.X+boxGap, boxTop, leftBox.Max.X+boxGap+boxW, boxBottom)
	fillRoundedRect(img, leftBox, 18, color.RGBA{R: 27, G: 56, B: 88, A: 255})
	fillRoundedRect(img, rightBox, 18, color.RGBA{R: 24, G: 48, B: 76, A: 255})

	drawTextTop(img, faces.Label, leftBox.Min.X+24, leftBox.Min.Y+26, tr(lang, "img_today_suhoor_label"), subtitleColor)
	drawTextTop(img, faces.Time, leftBox.Min.X+24, leftBox.Min.Y+72, minutesToClock(day.SuhoorEnd), titleColor)

	drawTextTop(img, faces.Label, rightBox.Min.X+24, rightBox.Min.Y+26, tr(lang, "img_today_iftar_label"), subtitleColor)
	drawTextTop(img, faces.Time, rightBox.Min.X+24, rightBox.Min.Y+72, minutesToClock(day.Maghrib), titleColor)

	details := image.Rect(inner.Min.X+18, leftBox.Max.Y+16, inner.Max.X-18, leftBox.Max.Y+16+92)
	fillRoundedRect(img, details, 16, color.RGBA{R: 18, G: 40, B: 63, A: 255})

	drawTextTop(img, faces.Footer, details.Min.X+20, details.Min.Y+52, tr(lang, "img_today_footer"), subtitleColor)

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func renderReminderImage(region string, day int, ev eventSpec, loc *time.Location, lang string) ([]byte, error) {
	lang = normalizeLang(lang)
	if lang == "" {
		lang = langTG
	}
	faces, err := loadReminderCardFaces()
	if err != nil {
		return nil, err
	}
	defer faces.Close()

	const (
		imgW       = 980
		imgH       = 520
		margin     = 34
		cardRadius = 24
	)

	img := image.NewRGBA(image.Rect(0, 0, imgW, imgH))
	drawVerticalGradient(img, color.RGBA{R: 9, G: 19, B: 34, A: 255}, color.RGBA{R: 6, G: 13, B: 24, A: 255})
	drawRadialGlow(img, imgW-180, 110, 220, color.RGBA{R: 89, G: 188, B: 174, A: 90})
	drawRadialGlow(img, 150, imgH-90, 220, color.RGBA{R: 224, G: 174, B: 91, A: 65})

	card := image.Rect(margin, margin, imgW-margin, imgH-margin)
	shadow := image.Rect(card.Min.X+7, card.Min.Y+9, card.Max.X+7, card.Max.Y+9)
	fillRoundedRect(img, shadow, cardRadius, color.RGBA{R: 2, G: 6, B: 15, A: 120})
	fillRoundedRect(img, card, cardRadius, color.RGBA{R: 94, G: 121, B: 158, A: 255})

	inner := image.Rect(card.Min.X+2, card.Min.Y+2, card.Max.X-2, card.Max.Y-2)
	fillRoundedRect(img, inner, cardRadius-2, color.RGBA{R: 13, G: 25, B: 41, A: 255})

	header := image.Rect(inner.Min.X+18, inner.Min.Y+18, inner.Max.X-18, inner.Min.Y+128)
	fillRoundedRect(img, header, 18, color.RGBA{R: 26, G: 48, B: 76, A: 255})
	fillRoundedRect(
		img,
		image.Rect(header.Min.X+1, header.Min.Y+1, header.Max.X-1, header.Min.Y+header.Dy()/2),
		16,
		color.RGBA{R: 34, G: 63, B: 100, A: 255},
	)

	titleColor := color.RGBA{R: 243, G: 247, B: 252, A: 255}
	subtitleColor := color.RGBA{R: 176, G: 194, B: 214, A: 255}
	drawTextTop(img, faces.Title, header.Min.X+22, header.Min.Y+20, tr(lang, "img_rem_title"), titleColor)
	drawTextTop(img, faces.Subtitle, header.Min.X+22, header.Min.Y+64, tr(lang, "img_region_prefix")+region, subtitleColor)
	drawTextTop(
		img,
		faces.Subtitle,
		header.Min.X+22,
		header.Min.Y+90,
		trf(lang, "img_rem_day_date", day, ev.Time.In(loc).Format("02.01.2006")),
		subtitleColor,
	)

	eventBox := image.Rect(inner.Min.X+18, header.Max.Y+18, inner.Max.X-18, header.Max.Y+18+154)
	fillRoundedRect(img, eventBox, 18, color.RGBA{R: 24, G: 47, B: 74, A: 255})
	drawTextTop(img, faces.Event, eventBox.Min.X+24, eventBox.Min.Y+26, eventTitle(lang, ev), titleColor)
	drawTextTop(img, faces.Time, eventBox.Min.X+24, eventBox.Min.Y+74, ev.Time.In(loc).Format("15:04"), titleColor)

	footer := image.Rect(inner.Min.X+18, eventBox.Max.Y+14, inner.Max.X-18, eventBox.Max.Y+14+74)
	fillRoundedRect(img, footer, 15, color.RGBA{R: 18, G: 40, B: 63, A: 255})
	drawTextTop(img, faces.Footer, footer.Min.X+20, footer.Min.Y+24, tr(lang, "img_rem_footer"), subtitleColor)

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func drawVerticalGradient(img *image.RGBA, top, bottom color.RGBA) {
	bounds := img.Bounds()
	height := bounds.Dy()
	if height <= 1 {
		fillRect(img, bounds, top)
		return
	}
	for y := 0; y < height; y++ {
		r := int(top.R) + (int(bottom.R)-int(top.R))*y/(height-1)
		g := int(top.G) + (int(bottom.G)-int(top.G))*y/(height-1)
		bl := int(top.B) + (int(bottom.B)-int(top.B))*y/(height-1)
		a := int(top.A) + (int(bottom.A)-int(top.A))*y/(height-1)
		fillRect(
			img,
			image.Rect(bounds.Min.X, bounds.Min.Y+y, bounds.Max.X, bounds.Min.Y+y+1),
			color.RGBA{R: uint8(r), G: uint8(g), B: uint8(bl), A: uint8(a)},
		)
	}
}

type calendarCardFaces struct {
	Title       font.Face
	Subtitle    font.Face
	Badge       font.Face
	TableHeader font.Face
	TableRow    font.Face
	Footer      font.Face
}

func (f *calendarCardFaces) Close() {
	closeFace(f.Title)
	closeFace(f.Subtitle)
	closeFace(f.Badge)
	closeFace(f.TableHeader)
	closeFace(f.TableRow)
	closeFace(f.Footer)
}

type todayCardFaces struct {
	Title    font.Face
	Subtitle font.Face
	Badge    font.Face
	Label    font.Face
	Time     font.Face
	Footer   font.Face
}

func (f *todayCardFaces) Close() {
	closeFace(f.Title)
	closeFace(f.Subtitle)
	closeFace(f.Badge)
	closeFace(f.Label)
	closeFace(f.Time)
	closeFace(f.Footer)
}

type reminderCardFaces struct {
	Title    font.Face
	Subtitle font.Face
	Event    font.Face
	Time     font.Face
	Footer   font.Face
}

func (f *reminderCardFaces) Close() {
	closeFace(f.Title)
	closeFace(f.Subtitle)
	closeFace(f.Event)
	closeFace(f.Time)
	closeFace(f.Footer)
}

type fontWeight string

const (
	fontWeightRegular fontWeight = "regular"
	fontWeightMedium  fontWeight = "medium"
	fontWeightBold    fontWeight = "bold"
)

var (
	fontBytesMu     sync.Mutex
	fontBytesByKind = map[fontWeight][]byte{}
)

func loadTodayCardFaces() (*todayCardFaces, error) {
	title, err := newTextFace(fontWeightBold, 42, gobold.TTF)
	if err != nil {
		return nil, err
	}
	subtitle, err := newTextFace(fontWeightRegular, 24, goregular.TTF)
	if err != nil {
		closeFace(title)
		return nil, err
	}
	badge, err := newTextFace(fontWeightBold, 21, gobold.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		return nil, err
	}
	label, err := newTextFace(fontWeightMedium, 30, gomedium.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(badge)
		return nil, err
	}
	timeFace, err := newTextFace(fontWeightBold, 62, gobold.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(badge)
		closeFace(label)
		return nil, err
	}
	footer, err := newTextFace(fontWeightRegular, 22, goregular.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(badge)
		closeFace(label)
		closeFace(timeFace)
		return nil, err
	}

	return &todayCardFaces{
		Title:    title,
		Subtitle: subtitle,
		Badge:    badge,
		Label:    label,
		Time:     timeFace,
		Footer:   footer,
	}, nil
}

func loadReminderCardFaces() (*reminderCardFaces, error) {
	title, err := newTextFace(fontWeightBold, 38, gobold.TTF)
	if err != nil {
		return nil, err
	}
	subtitle, err := newTextFace(fontWeightRegular, 22, goregular.TTF)
	if err != nil {
		closeFace(title)
		return nil, err
	}
	event, err := newTextFace(fontWeightMedium, 33, gomedium.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		return nil, err
	}
	timeFace, err := newTextFace(fontWeightBold, 72, gobold.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(event)
		return nil, err
	}
	footer, err := newTextFace(fontWeightRegular, 21, goregular.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(event)
		closeFace(timeFace)
		return nil, err
	}

	return &reminderCardFaces{
		Title:    title,
		Subtitle: subtitle,
		Event:    event,
		Time:     timeFace,
		Footer:   footer,
	}, nil
}

func loadCalendarCardFaces() (*calendarCardFaces, error) {
	title, err := newTextFace(fontWeightBold, 36, gobold.TTF)
	if err != nil {
		return nil, err
	}
	subtitle, err := newTextFace(fontWeightRegular, 21, goregular.TTF)
	if err != nil {
		closeFace(title)
		return nil, err
	}
	badge, err := newTextFace(fontWeightBold, 19, gobold.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		return nil, err
	}
	tableHeader, err := newTextFace(fontWeightMedium, 20, gomedium.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(badge)
		return nil, err
	}
	tableRow, err := newTextFace(fontWeightRegular, 20, goregular.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(badge)
		closeFace(tableHeader)
		return nil, err
	}
	footer, err := newTextFace(fontWeightRegular, 18, goregular.TTF)
	if err != nil {
		closeFace(title)
		closeFace(subtitle)
		closeFace(badge)
		closeFace(tableHeader)
		closeFace(tableRow)
		return nil, err
	}
	return &calendarCardFaces{
		Title:       title,
		Subtitle:    subtitle,
		Badge:       badge,
		TableHeader: tableHeader,
		TableRow:    tableRow,
		Footer:      footer,
	}, nil
}

func newTextFace(weight fontWeight, size float64, fallback []byte) (font.Face, error) {
	if preferred := loadPreferredFontBytes(weight); len(preferred) > 0 {
		face, err := newOpenTypeFace(preferred, size)
		if err == nil {
			return face, nil
		}
		log.Printf("font fallback: cannot use preferred %s font: %v", weight, err)
	}
	return newOpenTypeFace(fallback, size)
}

func loadPreferredFontBytes(weight fontWeight) []byte {
	fontBytesMu.Lock()
	if bytes, ok := fontBytesByKind[weight]; ok {
		fontBytesMu.Unlock()
		return bytes
	}
	fontBytesMu.Unlock()

	bytes := findPreferredFontBytes(weight)

	fontBytesMu.Lock()
	fontBytesByKind[weight] = bytes
	fontBytesMu.Unlock()
	return bytes
}

func findPreferredFontBytes(weight fontWeight) []byte {
	for _, path := range preferredFontPaths(weight) {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if supportsTajikRunes(data) {
			return data
		}
	}
	return nil
}

func preferredFontPaths(weight fontWeight) []string {
	commonRegular := []string{
		"/System/Library/Fonts/Supplemental/Arial.ttf",
		"/Library/Fonts/Arial.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/TTF/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/noto/NotoSans-Regular.ttf",
	}
	commonBold := []string{
		"/System/Library/Fonts/Supplemental/Arial Bold.ttf",
		"/Library/Fonts/Arial Bold.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/truetype/noto/NotoSans-Bold.ttf",
		"/usr/share/fonts/noto/NotoSans-Bold.ttf",
	}

	switch weight {
	case fontWeightBold:
		return append([]string{
			os.Getenv("RAMADAN_FONT_BOLD"),
			os.Getenv("RAMADAN_FONT"),
		}, commonBold...)
	case fontWeightMedium:
		return append([]string{
			os.Getenv("RAMADAN_FONT_MEDIUM"),
			os.Getenv("RAMADAN_FONT"),
		}, append(commonBold, commonRegular...)...)
	default:
		return append([]string{
			os.Getenv("RAMADAN_FONT_REGULAR"),
			os.Getenv("RAMADAN_FONT"),
		}, commonRegular...)
	}
}

func supportsTajikRunes(ttf []byte) bool {
	parsed, err := sfnt.Parse(ttf)
	if err != nil {
		return false
	}
	var buf sfnt.Buffer
	for _, r := range []rune{'ӯ', 'қ', 'ғ', 'ҳ', 'ҷ', 'ӣ'} {
		idx, err := parsed.GlyphIndex(&buf, r)
		if err != nil || idx == 0 {
			return false
		}
	}
	return true
}

func newOpenTypeFace(ttf []byte, size float64) (font.Face, error) {
	parsed, err := opentype.Parse(ttf)
	if err != nil {
		return nil, err
	}
	return opentype.NewFace(parsed, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
}

func closeFace(face font.Face) {
	if face == nil {
		return
	}
	closer, ok := face.(interface{ Close() error })
	if ok {
		_ = closer.Close()
	}
}

var tajikToRussianImageReplacer = strings.NewReplacer(
	"Ҳ", "Х",
	"ҳ", "х",
	"Қ", "К",
	"қ", "к",
	"Ғ", "Г",
	"ғ", "г",
	"Ҷ", "Ч",
	"ҷ", "ч",
	"Ӣ", "И",
	"ӣ", "и",
	"Ӯ", "У",
	"ӯ", "у",
)

func normalizeImageText(text string) string {
	if text == "" {
		return ""
	}
	return tajikToRussianImageReplacer.Replace(text)
}

func drawTextTop(img *image.RGBA, face font.Face, x, top int, text string, clr color.RGBA) {
	text = normalizeImageText(text)
	if face == nil || text == "" {
		return
	}
	baseline := top + fixedToInt(face.Metrics().Ascent)
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(clr),
		Face: face,
		Dot:  fixed.P(x, baseline),
	}
	d.DrawString(text)
}

func measureTextWidth(face font.Face, text string) int {
	text = normalizeImageText(text)
	if face == nil || text == "" {
		return 0
	}
	return fixedToInt(font.MeasureString(face, text))
}

func faceLineHeight(face font.Face) int {
	if face == nil {
		return 0
	}
	m := face.Metrics()
	return fixedToInt(m.Ascent + m.Descent)
}

func fixedToInt(v fixed.Int26_6) int {
	if v <= 0 {
		return 0
	}
	return int((v + 63) >> 6)
}

func drawRadialGlow(img *image.RGBA, cx, cy, radius int, clr color.RGBA) {
	if radius <= 0 || clr.A == 0 {
		return
	}
	minX := cx - radius
	maxX := cx + radius
	minY := cy - radius
	maxY := cy + radius
	rad := float64(radius)
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			dx := float64(x - cx)
			dy := float64(y - cy)
			dist := math.Sqrt(dx*dx + dy*dy)
			if dist > rad {
				continue
			}
			t := 1.0 - dist/rad
			alpha := uint8(float64(clr.A) * t * t)
			if alpha == 0 {
				continue
			}
			blendPixel(img, x, y, color.RGBA{R: clr.R, G: clr.G, B: clr.B, A: alpha})
		}
	}
}

func fillRoundedRect(img *image.RGBA, rect image.Rectangle, radius int, clr color.RGBA) {
	clipped := rect.Intersect(img.Bounds())
	if clipped.Empty() {
		return
	}
	for y := clipped.Min.Y; y < clipped.Max.Y; y++ {
		for x := clipped.Min.X; x < clipped.Max.X; x++ {
			if pointInRoundedRect(x, y, rect, radius) {
				blendPixel(img, x, y, clr)
			}
		}
	}
}

func pointInRoundedRect(x, y int, rect image.Rectangle, radius int) bool {
	if x < rect.Min.X || x >= rect.Max.X || y < rect.Min.Y || y >= rect.Max.Y {
		return false
	}
	if radius <= 0 {
		return true
	}
	r := minInt(radius, minInt(rect.Dx(), rect.Dy())/2)
	if r <= 0 {
		return true
	}
	cx := clampInt(x, rect.Min.X+r, rect.Max.X-r-1)
	cy := clampInt(y, rect.Min.Y+r, rect.Max.Y-r-1)
	dx := x - cx
	dy := y - cy
	return dx*dx+dy*dy <= r*r
}

func blendPixel(img *image.RGBA, x, y int, src color.RGBA) {
	if !image.Pt(x, y).In(img.Bounds()) || src.A == 0 {
		return
	}
	if src.A == 255 {
		img.SetRGBA(x, y, src)
		return
	}
	dst := img.RGBAAt(x, y)
	a := int(src.A)
	ia := 255 - a
	img.SetRGBA(x, y, color.RGBA{
		R: uint8((int(src.R)*a + int(dst.R)*ia) / 255),
		G: uint8((int(src.G)*a + int(dst.G)*ia) / 255),
		B: uint8((int(src.B)*a + int(dst.B)*ia) / 255),
		A: 255,
	})
}

func clampInt(v, minV, maxV int) int {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fillRect(img *image.RGBA, rect image.Rectangle, clr color.RGBA) {
	clipped := rect.Intersect(img.Bounds())
	if clipped.Empty() {
		return
	}
	draw.Draw(img, clipped, &image.Uniform{C: clr}, image.Point{}, draw.Src)
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

	dayIndex := int(math.Floor(now.Sub(start).Hours()/24.0)) + 1
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

func sampleHadithsByLang() map[string][]string {
	return map[string][]string{
		langTG: {
			"\"Рӯза сипар аст\" — ҳадис аз Паёмбар ﷺ (Бухорӣ).",
			"\"Касе ки бо имон ва барои ризои Аллоҳ дар Рамазон рӯза бигирад, гуноҳҳои гузаштааш бахшида мешаванд\" — ҳадис аз Абӯҳурайра (Бухорӣ, Муслим).",
			"\"Барои рӯзадор ду шодӣ ҳаст: ҳангоми ифтор ва ҳангоми мулоқоти Парвардигораш\" — ҳадис аз Абӯҳурайра (Бухорӣ).",
			"\"Дуои рӯзадор ҳангоми ифтор рад карда намешавад\" — ҳадис (Тирмизӣ).",
			"\"Амалҳо ба ниятҳо вобастаанд\" — ҳадис аз Паёмбар ﷺ (Бухорӣ).",
			"\"Беҳтарини шумо касест, ки Қуръонро омӯзад ва ба дигарон омӯзонад\" — ҳадис аз Паёмбар ﷺ (Бухорӣ).",
			"\"Покизагӣ нисфи имон аст\" — ҳадис аз Паёмбар ﷺ (Муслим).",
			"\"Дуо мағзи ибодат аст\" — ҳадис аз Паёмбар ﷺ (Тирмизӣ).",
			"\"Мусалмон бародари мусалмон аст\" — ҳадис аз Паёмбар ﷺ (Муслим).",
			"\"Талаби илм бар ҳар мусалмон фарз аст\" — ҳадис аз Паёмбар ﷺ (Ибни Моҷа).",
		},
		langRU: {
			"\"Пост — это щит\" — хадис Пророка ﷺ (Бухари).",
			"\"Кто постится в Рамадан с верой и надеждой на награду, тому простятся прежние грехи\" — хадис от Абу Хурайры (Бухари, Муслим).",
			"\"У постящегося две радости: при разговении и при встрече со своим Господом\" — хадис от Абу Хурайры (Бухари).",
			"\"Дуа постящегося во время ифтара не отвергается\" — хадис (Тирмизи).",
			"\"Дела оцениваются по намерениям\" — хадис Пророка ﷺ (Бухари).",
			"\"Лучший из вас тот, кто изучает Коран и обучает ему других\" — хадис Пророка ﷺ (Бухари).",
			"\"Чистота — половина веры\" — хадис Пророка ﷺ (Муслим).",
			"\"Дуа — суть поклонения\" — хадис Пророка ﷺ (Тирмизи).",
			"\"Мусульманин — брат мусульманину\" — хадис Пророка ﷺ (Муслим).",
			"\"Стремление к знанию обязательно для каждого мусульманина\" — хадис Пророка ﷺ (Ибн Маджа).",
		},
		langEN: {
			"\"Fasting is a shield\" — Hadith of the Prophet ﷺ (Bukhari).",
			"\"Whoever fasts Ramadan with faith and seeking reward, his previous sins will be forgiven\" — Hadith from Abu Huraira (Bukhari, Muslim).",
			"\"The fasting person has two joys: at iftar and when meeting his Lord\" — Hadith from Abu Huraira (Bukhari).",
			"\"The dua of the fasting person at iftar is not rejected\" — Hadith (Tirmidhi).",
			"\"Actions are judged by intentions\" — Hadith of the Prophet ﷺ (Bukhari).",
			"\"The best among you are those who learn the Quran and teach it\" — Hadith of the Prophet ﷺ (Bukhari).",
			"\"Purity is half of faith\" — Hadith of the Prophet ﷺ (Muslim).",
			"\"Supplication is the essence of worship\" — Hadith of the Prophet ﷺ (Tirmidhi).",
			"\"A Muslim is a brother to a Muslim\" — Hadith of the Prophet ﷺ (Muslim).",
			"\"Seeking knowledge is obligatory for every Muslim\" — Hadith of the Prophet ﷺ (Ibn Majah).",
		},
		langUZ: {
			"\"Ro‘za qalqondir\" — Payg‘ambar ﷺ hadisi (Buxoriy).",
			"\"Kim Ramazonda imon bilan va savob umidida ro‘za tutsa, avvalgi gunohlari kechiriladi\" — Abu Hurayra rivoyati (Buxoriy, Muslim).",
			"\"Ro‘zador uchun ikki xursandchilik bor: iftor paytida va Robbisi bilan uchrashganda\" — Abu Hurayra rivoyati (Buxoriy).",
			"\"Ro‘zadorning iftor paytidagi duosi rad etilmaydi\" — hadis (Termiziy).",
			"\"Amallar niyatlarga bog‘liq\" — Payg‘ambar ﷺ hadisi (Buxoriy).",
			"\"Sizlarning eng yaxshingiz Qur’onni o‘rganib, boshqalarga o‘rgatganingizdir\" — Payg‘ambar ﷺ hadisi (Buxoriy).",
			"\"Poklik iymonning yarmidir\" — Payg‘ambar ﷺ hadisi (Muslim).",
			"\"Duo ibodatning mag‘zidir\" — Payg‘ambar ﷺ hadisi (Termiziy).",
			"\"Musulmon musulmonning birodaridir\" — Payg‘ambar ﷺ hadisi (Muslim).",
			"\"Ilm talab qilish har bir musulmon uchun farzdir\" — Payg‘ambar ﷺ hadisi (Ibn Moja).",
		},
	}
}

func niyatTextsByLang() (map[string]string, map[string]string) {
	niyatSuhoor := map[string]string{
		langTG: `Нияти Рӯзаи моҳи шарифи Рамазон
Ба забони Арабӣ:
Валисавми ғаддин мин шаҳри рамазоналлазӣ фаризатан навайту.
Бо забони Тоҷикӣ:
Ният кардам рӯзаи моҳи шарифи Рамазон аз субҳи содиқ то фурӯ рафтани офтоб.`,
		langRU: `Ният на пост месяца Рамадан
На арабском:
Валисавми гаддин мин шаҳри рамазоналлазӣ фаризатан навайту.
На русском:
Я намереваюсь держать обязательный пост месяца Рамадан от рассвета до заката ради довольства Аллаха.`,
		langEN: `Niyyah for Ramadan fasting
In Arabic:
Wabisawmi ghadin min shahri ramadanal-ladhi faridatan nawaytu.
In English:
I intend to observe the obligatory fast of Ramadan from true dawn until sunset for the sake of Allah.`,
		langUZ: `Ramazon ro‘zasi uchun niyat
Arabcha:
Valisavmi g‘addin min shahri ramazonallaziy farizatan nawaytu.
O‘zbekcha:
Alloh rizoligi uchun Ramazon oyining farz ro‘zasini tongdan quyosh botguncha tutishga niyat qildim.`,
	}

	niyatIftar := map[string]string{
		langTG: `Дуъои Ифтор (кушодани рӯза):
Ба забони Арабӣ:
Аллоҳума лака сумту ва бика оманту ва алайка таваккалту ва ало ризқиқа афтарту. Бираҳматика ё арҳамар роҳимин.
Бо забони Тоҷикӣ:
Парвардигоро! Барои ризогии Ту рӯза доштам ва ба Ту имон овардам ва ба Ту такя дорам ва бо ризқи додаи Ту ифтор кардам.`,
		langRU: `Дуа ифтара (разговения):
На арабском:
Аллахумма лака сумту ва бика аманту ва ‘аляйка таваккалту ва ‘аля ризкыкя афтарту. Бирахматика я архамар-рахимин.
На русском:
О Аллах! Ради Тебя я постился, в Тебя уверовал, на Тебя уповаю и Твоим уделом разговелся.`,
		langEN: `Iftar dua:
In Arabic:
Allahumma laka sumtu wa bika amantu wa 'alayka tawakkaltu wa 'ala rizqika aftartu. Birahmatika ya arhamar-rahimin.
In English:
O Allah, for You I fasted, in You I believe, upon You I rely, and with Your provision I break my fast.`,
		langUZ: `Iftor duosi:
Arabcha:
Allohumma laka sumtu va bika omantu va alayka tavakkaltu va alo rizqika aftartu. Birohmatika ya arhamar-rohimin.
O‘zbekcha:
Parvardigor! Sening rizoliging uchun ro‘za tutdim, Senga iymon keltirdim, Senga tavakkal qildim va Sen bergan rizq bilan iftor qildim.`,
	}

	return niyatSuhoor, niyatIftar
}
