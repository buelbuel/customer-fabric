package profiles

import (
	"slices"
	"testing"
)

func ptr(s string) *string { return &s }

func TestPreferencesNormalize(t *testing.T) {
	got, err := Preferences{DisplayName: ptr("  Zoë Weiß "), PreferredLanguage: ptr("de-de"), Timezone: ptr("Europe/Berlin")}.Normalize()
	if err != nil || *got.DisplayName != "Zoë Weiß" || *got.PreferredLanguage != "de-DE" || *got.Timezone != "Europe/Berlin" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _ := (Preferences{DisplayName: ptr("  ")}).Normalize(); got.DisplayName != nil {
		t.Fatal("a blank value must clear the field")
	}
	for _, bad := range []Preferences{
		{DisplayName: ptr("bad\nname")},
		{DisplayName: ptr(string(make([]rune, 101)))},
		{PreferredLanguage: ptr("not a tag")},
		{Timezone: ptr("Mars/Olympus")},
		{Timezone: ptr("Local")},
	} {
		if _, err := bad.Normalize(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestPreferencesChanged(t *testing.T) {
	prev := Preferences{DisplayName: ptr("Ada"), Timezone: ptr("Europe/Berlin")}
	next := Preferences{DisplayName: ptr("Ada"), PreferredLanguage: ptr("en"), Timezone: nil}
	if got := next.Changed(prev); !slices.Equal(got, []string{"preferred_language", "timezone"}) {
		t.Fatalf("%v", got)
	}
	if got := prev.Changed(prev); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
