package main

import (
	"reflect"
	"testing"
	"time"
)

func TestImagesToPrune(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	at := func(daysAgo int) time.Time { return base.AddDate(0, 0, -daysAgo) }

	t.Run("referenced ID never emitted", func(t *testing.T) {
		onDisk := []imgMeta{
			{Ref: "repo:new", ID: "sha256:aaa", Tagged: true, LastSeen: at(0)},
			{Ref: "repo:old", ID: "sha256:bbb", Tagged: true, LastSeen: at(5)},
		}
		got := imagesToPrune(onDisk, map[string]bool{}, map[string]bool{"sha256:bbb": true}, 1)
		if len(got) != 0 {
			t.Fatalf("imagesToPrune = %v, want empty (ID protected)", got)
		}
	})

	t.Run("stable ref never emitted", func(t *testing.T) {
		onDisk := []imgMeta{
			{Ref: "repo:new", ID: "sha256:aaa", Tagged: true, LastSeen: at(0)},
			{Ref: "repo:stable", ID: "sha256:bbb", Tagged: true, LastSeen: at(5)},
			{Ref: "repo:old", ID: "sha256:ccc", Tagged: true, LastSeen: at(9)},
		}
		got := imagesToPrune(onDisk, map[string]bool{"repo:stable": true}, map[string]bool{}, 1)
		if want := []string{"repo:old"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("imagesToPrune = %v, want %v", got, want)
		}
	})

	t.Run("keepN keeps newest by LastSeen", func(t *testing.T) {
		onDisk := []imgMeta{
			{Ref: "repo:c", ID: "sha256:ccc", Tagged: true, LastSeen: at(3)},
			{Ref: "repo:a", ID: "sha256:aaa", Tagged: true, LastSeen: at(0)},
			{Ref: "repo:d", ID: "sha256:ddd", Tagged: true, LastSeen: at(7)},
			{Ref: "repo:b", ID: "sha256:bbb", Tagged: true, LastSeen: at(1)},
		}
		got := imagesToPrune(onDisk, map[string]bool{}, map[string]bool{}, 2)
		if want := []string{"repo:c", "repo:d"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("imagesToPrune = %v, want %v (two newest kept)", got, want)
		}
	})

	t.Run("dangling emits ID token", func(t *testing.T) {
		onDisk := []imgMeta{
			{Ref: "repo:new", ID: "sha256:aaa", Tagged: true, LastSeen: at(0)},
			{Ref: "repo@sha256:deadbeef", ID: "sha256:bbb", Tagged: false, LastSeen: at(5)},
		}
		got := imagesToPrune(onDisk, map[string]bool{}, map[string]bool{}, 1)
		if want := []string{"sha256:bbb"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("imagesToPrune = %v, want %v (dangling deletes by ID)", got, want)
		}
	})

	t.Run("Created fallback when LastSeen zero", func(t *testing.T) {
		onDisk := []imgMeta{
			{Ref: "repo:untracked-new", ID: "sha256:aaa", Tagged: true, Created: at(1).Unix()},
			{Ref: "repo:untracked-old", ID: "sha256:bbb", Tagged: true, Created: at(8).Unix()},
			{Ref: "repo:tracked", ID: "sha256:ccc", Tagged: true, LastSeen: at(4)},
		}
		got := imagesToPrune(onDisk, map[string]bool{}, map[string]bool{}, 1)
		// Order: untracked-new (Created 1d ago) kept, then tracked (4d), untracked-old (8d).
		if want := []string{"repo:tracked", "repo:untracked-old"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("imagesToPrune = %v, want %v (Created fallback ordering)", got, want)
		}
	})

	t.Run("keepN zero prunes all unprotected", func(t *testing.T) {
		onDisk := []imgMeta{
			{Ref: "repo:a", ID: "sha256:aaa", Tagged: true, LastSeen: at(0)},
			{Ref: "repo:b", ID: "sha256:bbb", Tagged: true, LastSeen: at(1)},
		}
		got := imagesToPrune(onDisk, map[string]bool{"repo:a": true}, map[string]bool{}, 0)
		if want := []string{"repo:b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("imagesToPrune = %v, want %v", got, want)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if got := imagesToPrune(nil, map[string]bool{}, map[string]bool{}, 3); len(got) != 0 {
			t.Fatalf("imagesToPrune(nil) = %v, want empty", got)
		}
	})
}
