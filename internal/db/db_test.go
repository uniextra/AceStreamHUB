package db

import "testing"

func TestDBSaveAndConsecutiveFails(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer store.Close()

	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.SaveScan(id, false, 1.0, "")
	store.SaveScan(id, false, 1.0, "")

	scans := store.GetAllScans()
	rec, ok := scans[id]
	if !ok {
		t.Fatalf("expected record for %s", id)
	}
	if rec.ConsecutiveFails != 2 || rec.Success {
		t.Fatalf("expected 2 consecutive fails and success=false, got %+v", rec)
	}

	store.SaveScan(id, true, 2.3, "FHD | 4.8 Mbps | Peers: 15")
	scans = store.GetAllScans()
	rec = scans[id]
	if !rec.Success || rec.ConsecutiveFails != 0 || rec.Resolution != "FHD" || rec.Peers != "15" {
		t.Fatalf("expected reset consecutive_fails and parsed info, got %+v", rec)
	}
}
