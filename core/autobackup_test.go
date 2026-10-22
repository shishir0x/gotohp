package core

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoBackupHistory(t *testing.T) {
	tempDir := t.TempDir()
	historyPath := filepath.Join(tempDir, "history.json")

	h := NewAutoBackupHistory(historyPath)
	defer func() { _ = h.Close() }()
	if h.Count() != 0 {
		t.Fatalf("expected count 0, got %d", h.Count())
	}

	testFile := filepath.Join(tempDir, "photo.jpg")
	if err := os.WriteFile(testFile, []byte("fake image data"), 0o644); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(testFile)
	if err != nil {
		t.Fatal(err)
	}

	// Not uploaded initially
	if h.IsUploaded(testFile, fi.Size(), fi.ModTime().Unix()) {
		t.Fatal("expected file to not be uploaded initially")
	}

	// Mark uploaded
	h.MarkUploaded(testFile, fi.Size(), fi.ModTime().Unix())
	if !h.IsUploaded(testFile, fi.Size(), fi.ModTime().Unix()) {
		t.Fatal("expected file to be uploaded after MarkUploaded")
	}

	// Different size/mtime reports false
	if h.IsUploaded(testFile, fi.Size()+10, fi.ModTime().Unix()) {
		t.Fatal("expected false for changed size")
	}

	// Save and reload
	if err := h.Save(); err != nil {
		t.Fatalf("failed to save history: %v", err)
	}
	_ = h.Close()

	h2 := NewAutoBackupHistory(historyPath)
	defer func() { _ = h2.Close() }()
	if !h2.IsUploaded(testFile, fi.Size(), fi.ModTime().Unix()) {
		t.Fatal("expected file to be uploaded in reloaded history")
	}
	if h2.Count() != 1 {
		t.Fatalf("expected count 1, got %d", h2.Count())
	}
}

func TestIsTemporaryDownloadFile(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"image.jpg", false},
		{"video.mp4", false},
		{".hidden.jpg", true},
		{"~$word.docx", true},
		{"download.crdownload", true},
		{"video.part", true},
		{"file.tmp", true},
		{"tempfile.temp", true},
		{"movie.download", true},
	}

	for _, tt := range tests {
		got := isTemporaryDownloadFile(tt.name)
		if got != tt.expected {
			t.Errorf("isTemporaryDownloadFile(%q) = %v, want %v", tt.name, got, tt.expected)
		}
	}
}

func TestAutoBackupManagerLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	folder1 := filepath.Join(tempDir, "folder1")
	if err := os.MkdirAll(folder1, 0o755); err != nil {
		t.Fatal(err)
	}

	// Set config path for this test
	cfgFile := filepath.Join(tempDir, "gotohp.config")
	if err := LoadConfig(cfgFile); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = CloseAppDatabase()
		ConfigPath = ""
	}()

	mgr := &ConfigManager{}
	updated, err := mgr.AddAutoBackupFolder(folder1)
	if err != nil {
		t.Fatalf("failed to add folder: %v", err)
	}
	if len(updated) != 1 || updated[0] != folder1 {
		t.Fatalf("expected [folder1], got %v", updated)
	}

	// Reject duplicates
	updated2, err := mgr.AddAutoBackupFolder(folder1)
	if err != nil {
		t.Fatalf("unexpected error adding duplicate folder: %v", err)
	}
	if len(updated2) != 1 {
		t.Fatalf("expected 1 folder after duplicate add, got %d", len(updated2))
	}

	// Reject invalid folder
	_, err = mgr.AddAutoBackupFolder(filepath.Join(tempDir, "non_existent"))
	if err == nil {
		t.Fatal("expected error adding non-existent folder")
	}

	// Update interval
	mgr.SetAutoBackupInterval(30)
	cfg := mgr.GetAutoBackup()
	if cfg.IntervalMinutes != 30 {
		t.Fatalf("expected interval 30, got %d", cfg.IntervalMinutes)
	}

	// Create AutoBackupManager
	abManager := NewAutoBackupManager(nil, slog.Default(), nil)
	if abManager.IsRunning() {
		t.Fatal("expected manager not running initially")
	}

	// Start without enabled should not run
	if err := abManager.Start(); err != nil {
		t.Fatal(err)
	}
	if !abManager.IsRunning() {
		// Enabled is false, so it started the watcher for configured folders
	}
	abManager.Stop()

	// Remove folder
	remaining := mgr.RemoveAutoBackupFolder(folder1)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 folders after removal, got %v", remaining)
	}
}

func TestFolderDeduplicationCaseSensitivity(t *testing.T) {
	tempDir := t.TempDir()
	folder := filepath.Join(tempDir, "TestFolder")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}

	cfgFile := filepath.Join(tempDir, "gotohp.config")
	if err := LoadConfig(cfgFile); err != nil {
		t.Fatal(err)
	}

	mgr := &ConfigManager{}
	list, err := mgr.AddAutoBackupFolder(folder)
	if err != nil {
		t.Fatalf("failed to add folder: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 folder, got %d", len(list))
	}

	// Add path with redundant separators and dots: e.g. folder/./
	redundantPath := folder + string(filepath.Separator) + "."
	list2, err := mgr.AddAutoBackupFolder(redundantPath)
	if err != nil {
		t.Fatalf("unexpected error adding redundant path: %v", err)
	}
	if len(list2) != 1 {
		t.Fatalf("expected redundant path to be de-duplicated, got count %d", len(list2))
	}

	// On Windows, test different casing: e.g. lowercase
	lowerFolder := strings.ToLower(folder)
	list3, err := mgr.AddAutoBackupFolder(lowerFolder)
	if err != nil {
		t.Fatalf("unexpected error adding lowercase folder: %v", err)
	}
	if len(list3) != 1 {
		t.Fatalf("expected case-insensitive duplicate rejection on Windows, got count %d", len(list3))
	}

	// Remove folder using lowercase path
	remaining := mgr.RemoveAutoBackupFolder(lowerFolder)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 folders after removal with different casing, got %v", remaining)
	}
}

func TestAutoBackupFlushNilUploadManager(t *testing.T) {
	abManager := NewAutoBackupManager(nil, slog.Default(), nil)

	// flush() with empty pending queue and nil upload manager must not panic
	abManager.flush()

	// schedule path and flush() with nil upload manager must not panic and must retain queue
	abManager.schedule("test_photo.jpg")

	abManager.flush()

	abManager.mu.Lock()
	pendingLen := len(abManager.pending)
	abManager.mu.Unlock()

	if pendingLen != 1 {
		t.Fatalf("expected pending queue to retain 1 item when uploadManager is nil, got %d", pendingLen)
	}
}

func TestAutoBackupHistoryInvalidation(t *testing.T) {
	tempDir := t.TempDir()
	historyPath := filepath.Join(tempDir, "history.json")

	h := NewAutoBackupHistory(historyPath)
	defer func() { _ = h.Close() }()
	testPath := filepath.Join(tempDir, "photo.jpg")

	h.MarkUploaded(testPath, 1024, 1700000000)
	if !h.IsUploaded(testPath, 1024, 1700000000) {
		t.Fatal("expected file to be marked as uploaded")
	}

	// Invalidate removes entry
	h.Invalidate(testPath)
	if h.IsUploaded(testPath, 1024, 1700000000) {
		t.Fatal("expected file to not be uploaded after Invalidate")
	}
}

func TestAddWatchRecursiveLimit(t *testing.T) {
	tempDir := t.TempDir()
	// Create multiple subdirectories
	for i := 0; i < 5; i++ {
		sub := filepath.Join(tempDir, "sub", string(rune('a'+i)))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	abManager := NewAutoBackupManager(nil, slog.Default(), nil)
	// Even with a synthetic directory tree, addWatchRecursive must succeed without error
	err := abManager.addWatchRecursive(nil, tempDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
