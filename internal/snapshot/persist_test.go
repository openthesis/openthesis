package snapshot

import (
	"testing"
)

func TestSaveLoadTree(t *testing.T) {
	dir := t.TempDir()
	tr := NewTree()
	// Tree.Create(parentID, hypervisorID, timeNS, coverage, rngState, faults, faultKindMask)
	id1, _ := tr.Create(RootID, 1, 1000, 42, 0xA1, 0, 0)
	id2, _ := tr.Create(id1, 2, 2000, 84, 0xA2, 0, 0)

	path := dir + "/tree.json"
	if err := SaveTree(tr, path); err != nil {
		t.Fatal(err)
	}

	tr2, err := LoadTree(path)
	if err != nil {
		t.Fatal(err)
	}
	if tr2.Len() != 3 {
		t.Fatalf("want 3 nodes, got %d", tr2.Len())
	}
	path2 := tr2.PathToRoot(id2)
	if len(path2) != 3 {
		t.Fatalf("want path length 3, got %d: %v", len(path2), path2)
	}
	if path2[0] != RootID || path2[1] != id1 || path2[2] != id2 {
		t.Fatalf("wrong path: %v", path2)
	}
}
