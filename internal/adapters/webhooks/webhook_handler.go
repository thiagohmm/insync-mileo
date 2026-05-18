package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

type WebhookHandler struct {
	port            int
	callbackHandler func(*domain.PushNotification) error
	syncUseCase     domain.SyncUseCase
	repo            domain.Repository
}

func NewWebhookHandler(port int, repo domain.Repository, syncUseCase domain.SyncUseCase) *WebhookHandler {
	return &WebhookHandler{
		port:        port,
		repo:        repo,
		syncUseCase: syncUseCase,
	}
}

// HandleNotification processes incoming webhook notifications
func (h *WebhookHandler) HandleNotification(notification *domain.PushNotification) error {
	log.Printf("Received webhook notification: %s", notification.Changed)
	return h.syncUseCase.HandleWebhookNotification(notification)
}

// RegisterWebhook registers a webhook with the cloud provider
func (h *WebhookHandler) RegisterWebhook(ctx context.Context, config *domain.WebhookConfig) error {
	// In a real implementation, this would call the cloud provider's API
	// to register a webhook with the callback URL
	// For now, we just save the configuration to the database
	return h.repo.SaveWebhookConfig(ctx, config)
}

// UnregisterWebhook removes a webhook from the cloud provider
func (h *WebhookHandler) UnregisterWebhook(ctx context.Context, config *domain.WebhookConfig) error {
	// In a real implementation, this would call the cloud provider's API
	// to unsubscribe from webhook notifications
	return h.repo.DeleteWebhookConfig(ctx, config.ID)
}

// StartCallbackServer starts an HTTP server to receive webhook notifications
func (h *WebhookHandler) StartCallbackServer() error {
	mux := http.NewServeMux()
	server := &http.Server{Addr: fmt.Sprintf(":%d", h.port), Handler: mux}

	mux.HandleFunc("/", h.handleWebhookCallback)

	log.Printf("Webhook callback server started on port %d", h.port)
	return server.ListenAndServe()
}

// handleWebhookCallback handles incoming webhook notifications
func (h *WebhookHandler) handleWebhookCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the webhook notification
	var notification domain.PushNotification

	// For Google Drive, the body contains a JSON payload
	// For other providers, parsing may differ
	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
			log.Printf("Failed to decode webhook payload: %v", err)
			http.Error(w, "Invalid payload", http.StatusBadRequest)
			return
		}
	} else {
		// Handle form-encoded or other formats
		body := make(map[string]string)
		if err := r.ParseForm(); err == nil {
			for k, v := range r.Form {
				if len(v) > 0 {
					body[k] = v[0]
				}
			}
		}
		notification = domain.PushNotification{
			ResourceID: body["resourceId"],
			ChannelID:  body["channelId"],
			Changed:    body["id"],
			State:      body["state"],
		}
	}

	// Validate the notification
	if notification.Changed == "" {
		log.Println("Invalid webhook notification: missing changed resource")
		http.Error(w, "Invalid notification", http.StatusBadRequest)
		return
	}

	// Process the notification
	if err := h.HandleNotification(&notification); err != nil {
		log.Printf("Failed to process webhook notification: %v", err)
		http.Error(w, "Failed to process", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Webhook received")
}

// StartCallbackServerWithPort starts the webhook callback server on a specific port
func (h *WebhookHandler) StartCallbackServerWithPort(port int) error {
	mux := http.NewServeMux()
	server := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}

	mux.HandleFunc("/", h.handleWebhookCallback)

	log.Printf("Webhook callback server started on port %d", port)
	return server.ListenAndServe()
}

// CreateWebhookConfig creates a webhook configuration for a sync config
func (h *WebhookHandler) CreateWebhookConfig(ctx context.Context, syncConfigID int64, channelID, resourceID string) (*domain.WebhookConfig, error) {
	expiration := time.Now().Add(7 * 24 * time.Hour) // Default: 7 days

	config := &domain.WebhookConfig{
		SyncConfigID: syncConfigID,
		ChannelID:    channelID,
		ResourceID:   resourceID,
		Expiration:   expiration,
		EventType:    "push_notification",
	}

	if err := h.repo.SaveWebhookConfig(ctx, config); err != nil {
		return nil, fmt.Errorf("failed to save webhook config: %w", err)
	}

	return config, nil
}

// ParseGoogleDriveNotification parses a Google Drive push notification
func ParseGoogleDriveNotification(body []byte) (*domain.PushNotification, error) {
	// Google Drive notifications contain a JSON payload with resource information
	// Format: {"resourceId": "...", "channelId": "...", "id": "...", "state": "..."}
	
	var result struct {
		ResourceID string `json:"resourceId"`
		ChannelID  string `json:"channelId"`
		ID         string `json:"id"`
		State      string `json:"state"`
	}
	
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse Google Drive notification: %w", err)
	}
	
	return &domain.PushNotification{
		ResourceID: result.ResourceID,
		ChannelID:  result.ChannelID,
		Changed:    result.ID,
		State:      result.State,
	}, nil
}

// ParseOneDriveNotification parses a OneDrive push notification
func ParseOneDriveNotification(body []byte) (*domain.PushNotification, error) {
	// OneDrive notifications have a different format
	// Format: [{"resourceId": "...", "resource": "...", "changeType": "...", ...}]
	
	var results []struct {
		ResourceID string `json:"resourceId"`
		Resource   string `json:"resource"`
		ChangeType string `json:"changeType"`
	}
	
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("failed to parse OneDrive notification: %w", err)
	}
	
	if len(results) == 0 {
		return nil, fmt.Errorf("no notifications in payload")
	}
	
	// For simplicity, return the first notification
	result := results[0]
	
	// Extract the file/folder ID from the resource URL
	resourceID := extractResourceID(result.Resource)
	
	return &domain.PushNotification{
		ResourceID: result.ResourceID,
		Changed:    resourceID,
		State:      result.ChangeType,
	}, nil
}

// extractResourceID extracts the file/folder ID from a OneDrive resource URL
func extractResourceID(resourceURL string) string {
	// OneDrive resource URLs look like: https://graph.microsoft.com/v1.0/drives/{driveId}/items/{itemId}
	// We extract the itemId from the URL
	parts := strings.Split(resourceURL, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return resourceURL
}