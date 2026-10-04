package main

import (
	"reflect"
	"testing"
	"time"
)

func TestParseRefetchChatsDedupesAndTrims(t *testing.T) {
	got := parseRefetchChats(" 120363203127855640@g.us,120363184598656143@g.us,, 120363203127855640@g.us\n120363202469540309@g.us ")
	want := []string{"120363203127855640@g.us", "120363184598656143@g.us", "120363202469540309@g.us"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseRefetchChats = %#v, want %#v", got, want)
	}
}

func TestParseRefetchSince(t *testing.T) {
	got, err := parseRefetchSince("2026-01-01")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local); !got.Equal(want) {
		t.Fatalf("since = %v, want local midnight %v", got, want)
	}
	if got, err := parseRefetchSince(""); err != nil || !got.IsZero() {
		t.Fatalf("empty --since should mean no bound, got %v %v", got, err)
	}
	if _, err := parseRefetchSince("01/02/2026"); err == nil {
		t.Fatal("expected an error for a non-ISO date")
	}
}
