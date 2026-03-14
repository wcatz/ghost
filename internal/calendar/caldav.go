// Package calendar provides CalDAV calendar integration for Ghost.
// Queries today's events for briefings and upcoming meeting warnings.
package calendar

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// Event is a simplified calendar event.
type Event struct {
	Summary   string    `json:"summary"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Location  string    `json:"location,omitempty"`
	AllDay    bool      `json:"all_day"`
}

// Client queries CalDAV calendars.
type Client struct {
	caldav *caldav.Client
	logger *slog.Logger
	calPaths []string // discovered calendar paths
}

// Config holds CalDAV connection settings.
type Config struct {
	URL      string `koanf:"url"`
	Username string `koanf:"username"`
	Password string `koanf:"password"`
}

// NewClient creates a CalDAV client and discovers calendars.
func NewClient(ctx context.Context, cfg Config, logger *slog.Logger) (*Client, error) {
	httpClient := webdav.HTTPClientWithBasicAuth(nil, cfg.Username, cfg.Password)
	c, err := caldav.NewClient(httpClient, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("caldav client: %w", err)
	}

	client := &Client{
		caldav: c,
		logger: logger,
	}

	// Discover calendars.
	if err := client.discover(ctx); err != nil {
		logger.Warn("caldav discovery failed, will retry on query", "error", err)
	}

	return client, nil
}

func (c *Client) discover(ctx context.Context) error {
	principal, err := c.caldav.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return fmt.Errorf("find principal: %w", err)
	}

	calHome, err := c.caldav.FindCalendarHomeSet(ctx, principal)
	if err != nil {
		return fmt.Errorf("find calendar home: %w", err)
	}

	calendars, err := c.caldav.FindCalendars(ctx, calHome)
	if err != nil {
		return fmt.Errorf("find calendars: %w", err)
	}

	c.calPaths = make([]string, 0, len(calendars))
	for _, cal := range calendars {
		// Only include calendars that support VEVENT.
		for _, comp := range cal.SupportedComponentSet {
			if comp == "VEVENT" {
				c.calPaths = append(c.calPaths, cal.Path)
				c.logger.Info("calendar discovered", "name", cal.Name, "path", cal.Path)
				break
			}
		}
	}

	if len(c.calPaths) == 0 {
		return fmt.Errorf("no calendars with VEVENT support found")
	}
	return nil
}

// TodayEvents returns all events for today across all calendars.
func (c *Client) TodayEvents(ctx context.Context) ([]Event, error) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	end := start.Add(24 * time.Hour)
	return c.EventsInRange(ctx, start, end)
}

// UpcomingEvents returns events in the next N hours.
func (c *Client) UpcomingEvents(ctx context.Context, hours int) ([]Event, error) {
	now := time.Now()
	return c.EventsInRange(ctx, now, now.Add(time.Duration(hours)*time.Hour))
}

// EventsInRange queries all calendars for events in the given time range.
func (c *Client) EventsInRange(ctx context.Context, start, end time.Time) ([]Event, error) {
	if len(c.calPaths) == 0 {
		if err := c.discover(ctx); err != nil {
			return nil, err
		}
	}

	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:    "VCALENDAR",
			AllProps: true,
			Comps: []caldav.CalendarCompRequest{
				{Name: "VEVENT", AllProps: true},
			},
		},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{
					Name:  "VEVENT",
					Start: start.UTC(),
					End:   end.UTC(),
				},
			},
		},
	}

	var allEvents []Event

	for _, calPath := range c.calPaths {
		objects, err := c.caldav.QueryCalendar(ctx, calPath, query)
		if err != nil {
			c.logger.Warn("query calendar", "error", err, "path", calPath)
			continue
		}

		for _, obj := range objects {
			if obj.Data == nil {
				continue
			}
			for _, comp := range obj.Data.Children {
				if comp.Name != "VEVENT" {
					continue
				}

				event := Event{}

				if prop := comp.Props.Get("SUMMARY"); prop != nil {
					event.Summary = prop.Value
				}
				if prop := comp.Props.Get("LOCATION"); prop != nil {
					event.Location = prop.Value
				}
				if prop := comp.Props.Get("DTSTART"); prop != nil {
					event.StartTime = parseICal(prop.Value)
					// Check if all-day (DATE vs DATE-TIME).
					if params := prop.Params; params != nil {
						if v := params.Get("VALUE"); v == "DATE" {
							event.AllDay = true
						}
					}
				}
				if prop := comp.Props.Get("DTEND"); prop != nil {
					event.EndTime = parseICal(prop.Value)
				}

				if event.Summary != "" {
					allEvents = append(allEvents, event)
				}
			}
		}
	}

	sort.Slice(allEvents, func(i, j int) bool {
		return allEvents[i].StartTime.Before(allEvents[j].StartTime)
	})

	return allEvents, nil
}

// parseICal parses iCal datetime formats.
func parseICal(s string) time.Time {
	// Try common formats.
	for _, layout := range []string{
		"20060102T150405Z",
		"20060102T150405",
		"20060102",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
