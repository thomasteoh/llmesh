package logring

import (
	"log/slog"
	"slices"
	"testing"
)

// logTo writes one Info record to category via the ring handler only, keeping
// the stderr half of NewLogger out of the test output.
func logTo(s *Sink, category, msg string) {
	slog.New(s.newHandler(category, slog.LevelInfo)).Info(msg)
}

// Every category main.go builds a logger for must survive to the ring. These
// were dropped for "correlation" and "upstream": the records reached stderr, so
// nothing looked broken, but the admin UI's log view for those two subsystems
// was permanently empty.
func TestSink_KeepsEveryCategoryMainWiresUp(t *testing.T) {
	for _, cat := range []string{"router", "hub", "scheduler", "api", "correlation", "upstream", "admin"} {
		s := New(8)
		logTo(s, cat, "hello")
		if got := s.Query(cat, 10); len(got) != 1 {
			t.Errorf("category %q: got %d entries, want 1", cat, len(got))
		}
	}
}

// A category nobody pre-registered still records, so adding a subsystem cannot
// silently lose its logs.
func TestSink_RecordsUnregisteredCategory(t *testing.T) {
	s := New(8)
	logTo(s, "brand-new", "hello")

	got := s.Query("brand-new", 10)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].Message != "hello" {
		t.Errorf("message = %q, want %q", got[0].Message, "hello")
	}
}

// Categories drives the admin log view's category list, so a category that has
// logged must appear there — otherwise its entries are stored but unreachable,
// because the logs endpoint rejects any category the list omits.
func TestSink_CategoriesIncludesUnregistered(t *testing.T) {
	s := New(8)
	logTo(s, "zebra", "hello")
	logTo(s, "brand-new", "hello")

	cats := s.Categories()
	for _, want := range []string{"correlation", "upstream", "zebra", "brand-new"} {
		if !slices.Contains(cats, want) {
			t.Errorf("Categories() = %v, missing %q", cats, want)
		}
	}

	// Known categories keep their curated order and lead the list; the rest
	// follow sorted, so the UI ordering is stable across runs.
	if !slices.Equal(cats[:len(knownCategories)], knownCategories) {
		t.Errorf("Categories() = %v, want it to start with %v", cats, knownCategories)
	}
	if extra := cats[len(knownCategories):]; !slices.Equal(extra, []string{"brand-new", "zebra"}) {
		t.Errorf("unregistered categories = %v, want [brand-new zebra]", extra)
	}
}

// Categories must not hand out the slice backing the canonical order, or a
// caller appending to it corrupts every later call.
func TestSink_CategoriesDoesNotAliasKnown(t *testing.T) {
	s := New(8)
	got := s.Categories()
	got[0] = "clobbered"

	if knownCategories[0] == "clobbered" {
		t.Fatal("Categories() returned a slice aliasing knownCategories")
	}
	if again := s.Categories(); again[0] == "clobbered" {
		t.Error("a mutated result leaked into a later call")
	}
}
