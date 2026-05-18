package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/thiagohmm/insync-clone/api/proto/insync"
)

func TestSafeLocalName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "replace slashes",
			in:   "folder/file",
			want: "folder_file",
		},
		{
			name: "replace backslashes",
			in:   "folder\\file",
			want: "folder_file",
		},
		{
			name: "replace colons",
			in:   "file:with:colons",
			want: "file_with_colons",
		},
		{
			name: "multiple replacements",
			in:   "a/b\\c:d",
			want: "a_b_c_d",
		},
		{
			name: "trim spaces",
			in:   "  spaced  ",
			want: "spaced",
		},
		{
			name: "empty string defaults",
			in:   "",
			want: "sync-item",
		},
		{
			name: "dot defaults",
			in:   ".",
			want: "sync-item",
		},
		{
			name: "dotdot defaults",
			in:   "..",
			want: "sync-item",
		},
		{
			name: "normal name unchanged",
			in:   "normal-file.txt",
			want: "normal-file.txt",
		},
		{
			name: "only special chars",
			in:   "/\\:",
			want: "___",
		},
		{
			name: "only spaces defaults to sync-item",
			in:   "   ",
			want: "sync-item",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeLocalName(tt.in)
			if got != tt.want {
				t.Errorf("safeLocalName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultSyncRoot(t *testing.T) {
	got := defaultSyncRoot()

	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		want := filepath.Join(home, "Insync")
		if got != want {
			t.Errorf("defaultSyncRoot() = %q, want %q", got, want)
		}
	}
}

func TestDefaultSyncRootUsesAllowedRootEnv(t *testing.T) {
	root := filepath.Join(t.TempDir(), "allowed")
	t.Setenv(cliAllowedRootEnv, root)

	if got := defaultSyncRoot(); got != root {
		t.Fatalf("defaultSyncRoot() = %q, want %q", got, root)
	}
}

func TestBrowsingDownMovesOneItem(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{
			name: "down arrow",
			msg:  tea.KeyMsg{Type: tea.KeyDown},
		},
		{
			name: "j",
			msg:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := browsingTestModel()
			updated, _ := m.Update(tt.msg)
			got := updated.(model).list.SelectedItem().(browseItem).title

			if got != "second.txt" {
				t.Fatalf("selected item after one %s = %q, want %q", tt.name, got, "second.txt")
			}
		})
	}
}

func TestFolderSelectionMarksChildren(t *testing.T) {
	tests := []struct {
		name      string
		selected  map[string]browseItem
		synced    map[string]insync.SyncMode
		wantTitle string
	}{
		{
			name: "pending selected folder",
			selected: map[string]browseItem{
				"folder-id": {
					remoteID:    "folder-id",
					isDirectory: true,
					mode:        insync.SyncMode_FULL_SYNC,
					hasMode:     true,
				},
			},
			wantTitle: "[F] child.txt",
		},
		{
			name:      "already synced folder",
			synced:    map[string]insync.SyncMode{"folder-id": insync.SyncMode_BASE_SYNC},
			wantTitle: "[B] child.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := model{
				list:          list.New([]list.Item{}, list.NewDefaultDelegate(), 80, 24),
				state:         stateBrowsing,
				browsePath:    []string{"root", "folder-id"},
				syncedModes:   make(map[string]insync.SyncMode),
				selectedItems: make(map[string]browseItem),
			}
			for id, item := range tt.selected {
				m.selectedItems[id] = item
			}
			for id, mode := range tt.synced {
				m.syncedModes[id] = mode
			}

			updated, _ := m.Update(fileListMsg{files: []*insync.FileInfo{
				{Name: "child.txt", Path: "child-id"},
			}})
			got := updated.(model).list.Items()[1].(browseItem).Title()

			if got != tt.wantTitle {
				t.Fatalf("child title = %q, want %q", got, tt.wantTitle)
			}
		})
	}
}

func TestSyncedMarkerSurvivesRefreshUntilUnsync(t *testing.T) {
	m := model{
		list:          list.New([]list.Item{}, list.NewDefaultDelegate(), 80, 24),
		state:         stateBrowsing,
		browsePath:    []string{"root"},
		syncedModes:   map[string]insync.SyncMode{"file-id": insync.SyncMode_FULL_SYNC},
		selectedItems: make(map[string]browseItem),
	}
	updated, _ := m.Update(syncedListMsg{modes: map[string]insync.SyncMode{}})
	m = updated.(model)

	if mode, ok := m.syncedModes["file-id"]; !ok || mode != insync.SyncMode_FULL_SYNC {
		t.Fatalf("synced marker was forgotten without unsync: mode=%v ok=%v", mode, ok)
	}

	delete(m.syncedModes, "file-id")
	updated, _ = m.Update(fileListMsg{files: []*insync.FileInfo{
		{Name: "file.txt", Path: "file-id"},
	}})
	got := updated.(model).list.Items()[0].(browseItem).Title()
	if got != "[] file.txt" {
		t.Fatalf("title after unsync = %q, want %q", got, "[] file.txt")
	}
}

func TestUnsyncTargetUsesInheritedFolder(t *testing.T) {
	m := model{
		browsePath:    []string{"root", "folder-id"},
		syncedModes:   map[string]insync.SyncMode{"folder-id": insync.SyncMode_BASE_SYNC},
		selectedItems: make(map[string]browseItem),
	}

	remoteID, _, ok := m.unsyncTarget(browseItem{title: "child.txt", remoteID: "child-id"})
	if !ok {
		t.Fatal("expected inherited child to be unsyncable")
	}
	if remoteID != "folder-id" {
		t.Fatalf("unsync target = %q, want folder-id", remoteID)
	}
}

func TestToggleSyncStartsImmediatelyWithConfiguredPath(t *testing.T) {
	localRoot := t.TempDir()
	t.Setenv(cliAllowedRootEnv, localRoot)
	m := browsingTestModel()
	m.lastLocalRoot = localRoot

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	got := updated.(model)

	if cmd == nil {
		t.Fatal("expected sync command to start immediately")
	}
	if got.lastLocalRoot != localRoot {
		t.Fatalf("lastLocalRoot = %q, want %q", got.lastLocalRoot, localRoot)
	}
	if got.status != "Enviando 1 item(ns) ao servidor..." {
		t.Fatalf("status = %q", got.status)
	}
	if mode, ok := got.syncedModes["first"]; !ok || mode != insync.SyncMode_BASE_SYNC {
		t.Fatalf("synced mode for first = %v ok=%v, want base-sync", mode, ok)
	}
}

func TestToggleSyncStartsImmediatelyWithDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := browsingTestModel()
	m.lastLocalRoot = ""

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	got := updated.(model)

	if cmd == nil {
		t.Fatal("expected sync command to start immediately")
	}
	if got.lastLocalRoot != defaultSyncRoot() {
		t.Fatalf("lastLocalRoot = %q, want default sync root %q", got.lastLocalRoot, defaultSyncRoot())
	}
	if mode, ok := got.syncedModes["first"]; !ok || mode != insync.SyncMode_FULL_SYNC {
		t.Fatalf("synced mode for first = %v ok=%v, want full-sync", mode, ok)
	}
}

func TestToggleSyncIgnoresLastLocalRootOutsideAllowedRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(cliAllowedRootEnv, filepath.Join(home, "Insync"))
	m := browsingTestModel()
	m.lastLocalRoot = filepath.Join(t.TempDir(), "old-sync-root")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	got := updated.(model)

	if cmd == nil {
		t.Fatal("expected sync command to start immediately")
	}
	want := filepath.Join(home, "Insync")
	if got.lastLocalRoot != want {
		t.Fatalf("lastLocalRoot = %q, want %q", got.lastLocalRoot, want)
	}
}

func TestToggleSyncUsesConfiguredPathWhenNoAllowedRootEnv(t *testing.T) {
	localRoot := filepath.Join(t.TempDir(), "sync")
	m := browsingTestModel()
	m.lastLocalRoot = localRoot

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	got := updated.(model)

	if cmd == nil {
		t.Fatal("expected sync command to start immediately")
	}
	if got.lastLocalRoot != localRoot {
		t.Fatalf("lastLocalRoot = %q, want %q", got.lastLocalRoot, localRoot)
	}
}

func browsingTestModel() model {
	m := model{
		list:          list.New([]list.Item{}, list.NewDefaultDelegate(), 80, 24),
		state:         stateBrowsing,
		browsePath:    []string{"root"},
		syncedModes:   make(map[string]insync.SyncMode),
		selectedItems: make(map[string]browseItem),
	}
	m.list.SetFilteringEnabled(true)
	m.list.SetShowFilter(true)
	m.list.DisableQuitKeybindings()
	updated, _ := m.Update(fileListMsg{files: []*insync.FileInfo{
		{Name: "first.txt", Path: "first"},
		{Name: "second.txt", Path: "second"},
		{Name: "third.txt", Path: "third"},
	}})
	return updated.(model)
}
