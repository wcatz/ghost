package google

import (
	"context"
	"fmt"
	"time"
)

// Email is a simplified email message.
type Email struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date"`
	Snippet string    `json:"snippet"`
	Unread  bool      `json:"unread"`
}

// UnreadCount returns the number of unread emails in the inbox.
func (c *Client) UnreadCount(ctx context.Context) (int, error) {
	label, err := c.Gmail.Users.Labels.Get("me", "INBOX").Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("get inbox label: %w", err)
	}
	return int(label.MessagesUnread), nil
}

// RecentUnread returns the most recent unread emails.
func (c *Client) RecentUnread(ctx context.Context, limit int) ([]Email, error) {
	resp, err := c.Gmail.Users.Messages.List("me").
		Q("is:unread in:inbox").
		MaxResults(int64(limit)).
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("list unread messages: %w", err)
	}

	emails := make([]Email, 0, len(resp.Messages))
	for _, msg := range resp.Messages {
		full, err := c.Gmail.Users.Messages.Get("me", msg.Id).
			Format("metadata").
			MetadataHeaders("From", "Subject", "Date").
			Context(ctx).
			Do()
		if err != nil {
			c.logger.Warn("get message", "error", err, "id", msg.Id)
			continue
		}

		email := Email{
			ID:      msg.Id,
			Snippet: full.Snippet,
			Unread:  true,
		}

		for _, hdr := range full.Payload.Headers {
			switch hdr.Name {
			case "From":
				email.From = hdr.Value
			case "Subject":
				email.Subject = hdr.Value
			case "Date":
				// Try common date formats.
				for _, layout := range []string{
					time.RFC1123Z,
					time.RFC1123,
					"Mon, 2 Jan 2006 15:04:05 -0700",
					"2 Jan 2006 15:04:05 -0700",
				} {
					if t, err := time.Parse(layout, hdr.Value); err == nil {
						email.Date = t
						break
					}
				}
			}
		}

		emails = append(emails, email)
	}

	return emails, nil
}
