package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// labTracker is a minimal client for the lab-tracker device-readings API,
// authenticated with a personal access token.
type labTracker struct {
	baseURL string
	token   string
	http    *http.Client
}

func newLabTracker(baseURL, token string) *labTracker {
	return &labTracker{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type ltProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ltReadingRef struct {
	ExternalID string `json:"externalId"`
	ReportID   string `json:"reportId"`
	ProfileID  string `json:"profileId"`
}

type ltResult struct {
	Analyte string  `json:"analyte"`
	Value   float64 `json:"value"`
	Unit    string  `json:"unit"`
}

type ltReading struct {
	ExternalID string     `json:"externalId"`
	ProfileID  string     `json:"profileId"`
	TakenAt    string     `json:"takenAt"`
	Device     string     `json:"device"`
	Results    []ltResult `json:"results"`
}

type ltImportResp struct {
	Status string `json:"status"` // "imported" or "exists"
	ltReadingRef
}

func (c *labTracker) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s (%d)", method, path, e.Error, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *labTracker) profiles(ctx context.Context) ([]ltProfile, error) {
	var out []ltProfile
	return out, c.do(ctx, http.MethodGet, "/api/profiles", nil, &out)
}

// imported returns the readings among ids that are already in lab-tracker,
// keyed by external id.
func (c *labTracker) imported(ctx context.Context, ids []string) (map[string]ltReadingRef, error) {
	var out struct {
		Imported []ltReadingRef `json:"imported"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/device-readings/lookup", map[string]any{"externalIds": ids}, &out); err != nil {
		return nil, err
	}
	m := make(map[string]ltReadingRef, len(out.Imported))
	for _, r := range out.Imported {
		m[r.ExternalID] = r
	}
	return m, nil
}

func (c *labTracker) importReading(ctx context.Context, r ltReading) (ltImportResp, error) {
	var out ltImportResp
	return out, c.do(ctx, http.MethodPost, "/api/device-readings", r, &out)
}
