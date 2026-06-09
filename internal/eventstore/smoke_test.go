package eventstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueryRoundtrip(t *testing.T) {
	dir := t.TempDir()
	es, err := Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		es.Append(Event{VTimeNS: uint64(i * 1000), Step: uint64(i), Type: TypeSDKAssert})
	}
	es.Append(Event{VTimeNS: 9999, Type: TypeSDKViolation})
	es.Close()

	evts, err := Query(filepath.Join(dir, "events.jsonl"), Filter{Types: []string{TypeSDKAssert}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evts) != 5 {
		t.Fatalf("want 5 assert events, got %d", len(evts))
	}

	evts2, _ := Query(filepath.Join(dir, "events.jsonl"), Filter{UpTo: 2500})
	if len(evts2) != 3 {
		t.Fatalf("want 3 events up_to 2500ns, got %d", len(evts2))
	}

	// Query non-existent file returns nil, nil.
	evts3, err := Query(filepath.Join(dir, "nonexistent.jsonl"), Filter{})
	if err != nil || evts3 != nil {
		t.Fatalf("want nil,nil for missing file, got %v %v", evts3, err)
	}
	os.Remove(filepath.Join(dir, "events.jsonl"))
}
