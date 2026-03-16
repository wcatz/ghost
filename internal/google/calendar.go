package google

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Event is a simplified calendar event.
type Event struct {
	Summary    string    `json:"summary"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Location   string    `json:"location,omitempty"`
	MeetLink   string    `json:"meet_link,omitempty"`
	AllDay     bool      `json:"all_day"`
	Attendees  []string  `json:"attendees,omitempty"`
	Organizer  string    `json:"organizer,omitempty"`
}

var meetURLRe = regexp.MustCompile(`https://meet\.google\.com/[a-z]{3}-[a-z]{4}-[a-z]{3}`)

// TodayEvents returns all events for today.
func (c *Client) TodayEvents(ctx context.Context) ([]Event, error) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.Add(24 * time.Hour)
	return c.EventsInRange(ctx, start, end)
}

// UpcomingEvents returns events in the next N minutes.
func (c *Client) UpcomingEvents(ctx context.Context, minutes int) ([]Event, error) {
	now := time.Now()
	return c.EventsInRange(ctx, now, now.Add(time.Duration(minutes)*time.Minute))
}

// EventsInRange queries Google Calendar for events in the given time range.
func (c *Client) EventsInRange(ctx context.Context, start, end time.Time) ([]Event, error) {
	items, err := c.Calendar.Events.List("primary").
		TimeMin(start.Format(time.RFC3339)).
		TimeMax(end.Format(time.RFC3339)).
		SingleEvents(true).
		OrderBy("startTime").
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("list calendar events: %w", err)
	}

	events := make([]Event, 0, len(items.Items))
	for _, item := range items.Items {
		ev := Event{
			Summary:  item.Summary,
			Location: item.Location,
		}

		// Parse start time.
		if item.Start.DateTime != "" {
			ev.Start, _ = time.Parse(time.RFC3339, item.Start.DateTime)
		} else if item.Start.Date != "" {
			ev.Start, _ = time.Parse("2006-01-02", item.Start.Date)
			ev.AllDay = true
		}

		// Parse end time.
		if item.End != nil {
			if item.End.DateTime != "" {
				ev.End, _ = time.Parse(time.RFC3339, item.End.DateTime)
			} else if item.End.Date != "" {
				ev.End, _ = time.Parse("2006-01-02", item.End.Date)
			}
		}

		// Extract Google Meet link.
		if item.ConferenceData != nil {
			for _, ep := range item.ConferenceData.EntryPoints {
				if ep.EntryPointType == "video" && strings.Contains(ep.Uri, "meet.google.com") {
					ev.MeetLink = ep.Uri
					break
				}
			}
		}
		// Fallback: check description for Meet URL.
		if ev.MeetLink == "" && item.Description != "" {
			if m := meetURLRe.FindString(item.Description); m != "" {
				ev.MeetLink = m
			}
		}
		// Fallback: hangout link.
		if ev.MeetLink == "" && item.HangoutLink != "" {
			ev.MeetLink = item.HangoutLink
		}

		// Extract attendees.
		if item.Attendees != nil {
			for _, a := range item.Attendees {
				name := a.DisplayName
				if name == "" {
					name = a.Email
				}
				if name != "" {
					ev.Attendees = append(ev.Attendees, name)
				}
			}
		}
		if item.Organizer != nil {
			ev.Organizer = item.Organizer.DisplayName
			if ev.Organizer == "" {
				ev.Organizer = item.Organizer.Email
			}
		}

		events = append(events, ev)
	}

	return events, nil
}
