package github

import (
	"time"

	gh "github.com/google/go-github/v68/github"
)

// buildGHNotification creates a go-github Notification for testing.
func buildGHNotification(id, repoFullName, title, subjectType, subjectURL, reason string, unread bool, updatedAt time.Time) *gh.Notification {
	ts := gh.Timestamp{Time: updatedAt}
	return &gh.Notification{
		ID: &id,
		Repository: &gh.Repository{
			FullName: &repoFullName,
		},
		Subject: &gh.NotificationSubject{
			Title: &title,
			URL:   &subjectURL,
			Type:  &subjectType,
		},
		Reason:    &reason,
		Unread:    &unread,
		UpdatedAt: &ts,
	}
}
