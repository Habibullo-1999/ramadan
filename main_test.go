package main

import (
	"strings"
	"testing"
	"time"
)

func dayByNumber(t *testing.T, days []DayTimes, day int) DayTimes {
	t.Helper()
	for _, d := range days {
		if d.Day == day {
			return d
		}
	}
	t.Fatalf("day %d not found", day)
	return DayTimes{}
}

func TestReminderDayBaseTime(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	start := time.Date(2026, time.February, 19, 0, 0, 0, 0, loc)

	gotDay1 := reminderDayBaseTime(start, 1, loc)
	wantDay1 := time.Date(2026, time.February, 19, 0, 0, 0, 0, loc)
	if !gotDay1.Equal(wantDay1) {
		t.Fatalf("day1 base mismatch: got %v want %v", gotDay1, wantDay1)
	}

	gotDay10 := reminderDayBaseTime(start, 10, loc)
	wantDay10 := time.Date(2026, time.February, 28, 0, 0, 0, 0, loc)
	if !gotDay10.Equal(wantDay10) {
		t.Fatalf("day10 base mismatch: got %v want %v", gotDay10, wantDay10)
	}
}

func TestReminderEventsForDay(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	base := time.Date(2026, time.February, 19, 0, 0, 0, 0, loc)
	day := DayTimes{
		Day:       1,
		SuhoorEnd: 341,  // 05:41
		Fajr:      341,  // 05:41
		Dhuhr:     761,  // 12:41
		Asr:       940,  // 15:40
		Maghrib:   1094, // 18:14
		Isha:      1170, // 19:30
	}

	events := reminderEventsForDay(base, day)
	if len(events) != 6 {
		t.Fatalf("expected 6 events, got %d", len(events))
	}

	tests := []struct {
		idx       int
		key       string
		minute    int
		useIftar  bool
		useSuhoor bool
	}{
		{idx: 0, key: "suhoor", minute: 341, useSuhoor: true},
		{idx: 1, key: "fajr", minute: 341},
		{idx: 2, key: "dhuhr", minute: 761},
		{idx: 3, key: "asr", minute: 940},
		{idx: 4, key: "maghrib", minute: 1094, useIftar: true},
		{idx: 5, key: "isha", minute: 1170},
	}
	for _, tc := range tests {
		ev := events[tc.idx]
		if ev.Key != tc.key {
			t.Fatalf("event %d key mismatch: got %q want %q", tc.idx, ev.Key, tc.key)
		}
		want := base.Add(time.Duration(tc.minute) * time.Minute)
		if !ev.Time.Equal(want) {
			t.Fatalf("event %q time mismatch: got %v want %v", ev.Key, ev.Time, want)
		}
		if ev.UseIftar != tc.useIftar {
			t.Fatalf("event %q UseIftar mismatch: got %v want %v", ev.Key, ev.UseIftar, tc.useIftar)
		}
		if ev.UseSuhoor != tc.useSuhoor {
			t.Fatalf("event %q UseSuhoor mismatch: got %v want %v", ev.Key, ev.UseSuhoor, tc.useSuhoor)
		}
	}
}

func TestShouldTriggerReminder(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	evTime := time.Date(2026, time.February, 19, 10, 0, 0, 0, loc)
	ev := eventSpec{Key: "fajr", Time: evTime}

	if shouldTriggerReminder(time.Date(2026, time.February, 19, 9, 29, 59, 0, loc), ev, map[string]bool{}) {
		t.Fatal("must not trigger before 30-minute mark")
	}
	if !shouldTriggerReminder(time.Date(2026, time.February, 19, 9, 30, 0, 0, loc), ev, map[string]bool{}) {
		t.Fatal("must trigger exactly at 30-minute mark")
	}
	if !shouldTriggerReminder(time.Date(2026, time.February, 19, 9, 45, 0, 0, loc), ev, map[string]bool{}) {
		t.Fatal("must trigger after 30-minute mark")
	}
	if shouldTriggerReminder(time.Date(2026, time.February, 19, 9, 45, 0, 0, loc), ev, map[string]bool{"fajr": true}) {
		t.Fatal("must not trigger for already sent event")
	}
}

func TestBuildCalendarsRegionOffset(t *testing.T) {
	cal := buildCalendars()
	dushanbe := cal["Душанбе"]
	asht := cal["Ашт"] // -6 offset in data table

	day1Dushanbe := dayByNumber(t, dushanbe, 1)
	day1Asht := dayByNumber(t, asht, 1)

	if day1Asht.Fajr != day1Dushanbe.Fajr-6 {
		t.Fatalf("fajr offset mismatch: got %d want %d", day1Asht.Fajr, day1Dushanbe.Fajr-6)
	}
	if day1Asht.Maghrib != day1Dushanbe.Maghrib-6 {
		t.Fatalf("maghrib offset mismatch: got %d want %d", day1Asht.Maghrib, day1Dushanbe.Maghrib-6)
	}
}

func TestCurrentDayScheduleBeforeStartReturnsDayZero(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	days := []DayTimes{
		{Day: 0, Data: "pre-start"},
		{Day: 1, Data: "day1"},
	}

	start := time.Now().In(loc).Add(12 * time.Hour)
	got := currentDaySchedule(days, start, loc)
	if got == nil {
		t.Fatal("expected day schedule, got nil")
	}
	if got.Day != 0 {
		t.Fatalf("expected day 0 before start, got day %d (%s)", got.Day, got.Data)
	}
}

func TestSendReminderUsesLocalizedNiyatAndTime(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	var sent string

	rm := &ReminderManager{
		loc:           loc,
		niyatSuhoor:   map[string]string{langEN: "EN_SUHOOR", langTG: "TG_SUHOOR"},
		niyatIftar:    map[string]string{langEN: "EN_IFTAR", langTG: "TG_IFTAR"},
		hadithsByLang: map[string][]string{langEN: {"EN_HADITH"}},
		getLangFn:     func(chatID int64) string { return langEN },
		sendFn: func(chatID int64, text string) error {
			sent = text
			return nil
		},
	}

	ev := eventSpec{
		Key:       "suhoor",
		Time:      time.Date(2026, time.February, 19, 5, 41, 0, 0, loc),
		UseSuhoor: true,
	}
	rm.sendReminder(1, "Dushanbe", 1, ev)

	if !strings.Contains(sent, "05:41") {
		t.Fatalf("expected reminder time in message, got: %q", sent)
	}
	if !strings.Contains(sent, tr(langEN, "niyat_suhoor_label")) {
		t.Fatalf("expected localized suhoor label, got: %q", sent)
	}
	if !strings.Contains(sent, "EN_SUHOOR") {
		t.Fatalf("expected localized suhoor niyat text, got: %q", sent)
	}
}
