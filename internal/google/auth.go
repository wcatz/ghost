// Package google provides OAuth2 authentication and API clients
// for Google Calendar and Gmail integration.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gcal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Config holds Google OAuth2 settings.
type Config struct {
	CredentialsFile string `koanf:"credentials_file"`
	TokenFile       string `koanf:"token_file"`
}

// Client provides authenticated access to Google APIs.
type Client struct {
	Calendar *gcal.Service
	Gmail    *gmail.Service
	logger   *slog.Logger
}

// NewClient creates a Google API client using OAuth2 credentials.
// On first run, it starts a local HTTP server and opens the browser
// for user consent. The resulting token is cached to disk.
func NewClient(ctx context.Context, cfg Config, logger *slog.Logger) (*Client, error) {
	credBytes, err := os.ReadFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}

	oauthCfg, err := google.ConfigFromJSON(credBytes,
		gcal.CalendarReadonlyScope,
		gmail.GmailReadonlyScope,
	)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}

	tok, err := loadToken(cfg.TokenFile)
	if err != nil {
		// No cached token — run the consent flow.
		logger.Info("no cached Google token, starting OAuth consent flow")
		tok, err = getTokenFromWeb(ctx, oauthCfg, logger)
		if err != nil {
			return nil, fmt.Errorf("oauth consent flow: %w", err)
		}
		if err := saveToken(cfg.TokenFile, tok); err != nil {
			logger.Warn("failed to cache token", "error", err)
		}
	}

	tokenSource := oauthCfg.TokenSource(ctx, tok)

	calSvc, err := gcal.NewService(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return nil, fmt.Errorf("create calendar service: %w", err)
	}

	gmailSvc, err := gmail.NewService(ctx, option.WithTokenSource(tokenSource))
	if err != nil {
		return nil, fmt.Errorf("create gmail service: %w", err)
	}

	logger.Info("google API client initialized")

	return &Client{
		Calendar: calSvc,
		Gmail:    gmailSvc,
		logger:   logger,
	}, nil
}

// getTokenFromWeb starts a local callback server, prints the auth URL,
// and waits for the OAuth2 callback.
func getTokenFromWeb(ctx context.Context, cfg *oauth2.Config, logger *slog.Logger) (*oauth2.Token, error) {
	// Find a free port for the callback.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for callback: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	cfg.RedirectURL = fmt.Sprintf("http://localhost:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- fmt.Errorf("no code in callback: %s", r.URL.Query().Get("error"))
			_, _ = fmt.Fprint(w, "<html><body><h2>Error — no authorization code received.</h2></body></html>")
			return
		}
		codeCh <- code
		_, _ = fmt.Fprint(w, "<html><body><h2>Ghost authorized! You can close this tab.</h2></body></html>")
	})

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	defer func() { _ = srv.Close() }()

	authURL := cfg.AuthCodeURL("ghost-state", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	logger.Info("open this URL in your browser to authorize Ghost", "url", authURL)
	fmt.Printf("\n🔐 Open this URL to authorize Ghost:\n\n  %s\n\n", authURL)

	select {
	case code := <-codeCh:
		tok, err := cfg.Exchange(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("exchange code for token: %w", err)
		}
		return tok, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("timed out waiting for authorization (5 min)")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var tok oauth2.Token
	if err := json.NewDecoder(f).Decode(&tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return json.NewEncoder(f).Encode(tok)
}
