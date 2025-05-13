package unit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/user/stream-audio-to-web/relay"
	"github.com/user/stream-audio-to-web/slog"
)

func TestNewRelayManager(t *testing.T) {
	// Create a temporary file for testing.
	tempDir := t.TempDir()

	configFile := filepath.Join(tempDir, "relay_list.json")

	// Test creating a new relay manager.
	manager := relay.NewRelayManager(configFile, slog.Default())
	if manager == nil {
		t.Fatal("Failed to create RelayManager")
	}

	// Verify initial state.
	if manager.IsActive() {
		t.Error("New RelayManager should be inactive by default")
	}

	links := manager.GetLinks()
	if len(links) != 0 {
		t.Errorf("New RelayManager should have an empty list, got %d items", len(links))
	}
}

func TestRelayManagerLoadAndSave(t *testing.T) {
	// Create a temporary file for testing.
	tempDir := t.TempDir()

	configFile := filepath.Join(tempDir, "relay_list.json")

	// Create a JSON file with test data.
	testLinks := []string{
		"http://example.com/stream1",
		"https://example.org/stream2",
	}
	data, err := json.MarshalIndent(testLinks, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal test data: %v", err)
	}

	if writeErr := os.WriteFile(configFile, data, 0644); writeErr != nil {
		t.Fatalf("Failed to write test config file: %v", writeErr)
	}

	// Create a new relay manager that should load the test data.
	manager := relay.NewRelayManager(configFile, slog.Default())

	// Verify links were loaded.
	links := manager.GetLinks()
	if len(links) != len(testLinks) {
		t.Errorf("Expected %d links, got %d", len(testLinks), len(links))
	}

	for i, link := range links {
		if link != testLinks[i] {
			t.Errorf("Expected link %q, got %q", testLinks[i], link)
		}
	}

	// Add a new link and verify it's saved.
	newLink := "https://example.net/stream3"
	if addErr := manager.AddLink(newLink); addErr != nil {
		t.Fatalf("Failed to add link: %v", addErr)
	}

	// Check if file contains updated data.
	fileData, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("Failed to read config file: %v", err)
	}

	var savedLinks []string
	if unmarshalErr := json.Unmarshal(fileData, &savedLinks); unmarshalErr != nil {
		t.Fatalf("Failed to unmarshal saved data: %v", unmarshalErr)
	}

	// Verify the new link was saved.
	found := false
	for _, link := range savedLinks {
		if link == newLink {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Added link %q was not saved to file", newLink)
	}
}

func TestRelayManagerAddLink(t *testing.T) {
	// Create a temporary file for testing.
	tempDir := t.TempDir()
	manager := relay.NewRelayManager(filepath.Join(tempDir, "relay_list.json"), slog.Default())

	// Test adding a valid link.
	validLink := "https://example.com/stream"
	if err := manager.AddLink(validLink); err != nil {
		t.Errorf("Failed to add valid link: %v", err)
	}

	links := manager.GetLinks()
	if len(links) != 1 || links[0] != validLink {
		t.Errorf("Link was not added correctly, got %v", links)
	}

	// Test adding an invalid link (no scheme).
	invalidLink := "example.com/stream"
	if err := manager.AddLink(invalidLink); err == nil {
		t.Error("Expected error when adding invalid link (no scheme), but got nil")
	}

	// Test adding a duplicate link.
	if err := manager.AddLink(validLink); err == nil {
		t.Error("Expected error when adding duplicate link, but got nil")
	}
}

func TestRelayManagerRemoveLink(t *testing.T) {
	// Create a temporary file for testing.
	tempDir := t.TempDir()
	manager := relay.NewRelayManager(filepath.Join(tempDir, "relay_list.json"), slog.Default())

	// Add some test links.
	links := []string{
		"http://example.com/stream1",
		"https://example.org/stream2",
		"https://example.net/stream3",
	}

	for _, link := range links {
		if err := manager.AddLink(link); err != nil {
			t.Fatalf("Failed to add link: %v", err)
		}
	}

	// Test removing a link by valid index.
	if err := manager.RemoveLink(1); err != nil {
		t.Errorf("Failed to remove link at index 1: %v", err)
	}

	// Verify the link was removed.
	currentLinks := manager.GetLinks()
	if len(currentLinks) != 2 {
		t.Errorf("Expected 2 links after removal, got %d", len(currentLinks))
	}

	// Verify the correct link was removed.
	for _, link := range currentLinks {
		if link == links[1] {
			t.Errorf("Link at index 1 (%q) was not removed", links[1])
		}
	}

	// Test removing a link with invalid index.
	invalidIndex := len(currentLinks) + 1
	if err := manager.RemoveLink(invalidIndex); err == nil {
		t.Errorf("Expected error when removing link with invalid index %d, but got nil", invalidIndex)
	}

	negativeIndex := -1
	if err := manager.RemoveLink(negativeIndex); err == nil {
		t.Errorf("Expected error when removing link with negative index %d, but got nil", negativeIndex)
	}
}

func TestRelayManagerActivation(t *testing.T) {
	// Create a temporary file for testing.
	tempDir := t.TempDir()
	manager := relay.NewRelayManager(filepath.Join(tempDir, "relay_list.json"), slog.Default())

	// Verify initial state.
	if manager.IsActive() {
		t.Error("New RelayManager should be inactive by default")
	}

	// Test activation.
	manager.SetActive(true)
	if !manager.IsActive() {
		t.Error("RelayManager should be active after SetActive(true)")
	}

	// Test deactivation.
	manager.SetActive(false)
	if manager.IsActive() {
		t.Error("RelayManager should be inactive after SetActive(false)")
	}
}
