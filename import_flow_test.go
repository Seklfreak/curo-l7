package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testToken = "lt_test-token"

// fakeLabTracker implements the three endpoints the importer uses, keeping
// imported readings in memory.
type fakeLabTracker struct {
	t        *testing.T
	mu       sync.Mutex
	profiles []ltProfile
	imported map[string]ltReading // by external id
	posts    []ltReading
	lookups  int
}

func newFakeLabTracker(t *testing.T) (*fakeLabTracker, *httptest.Server) {
	f := &fakeLabTracker{
		t:        t,
		profiles: []ltProfile{{ID: "p-alex", Name: "Alex"}, {ID: "p-sam", Name: "Sam"}},
		imported: map[string]ltReading{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeLabTracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid token"})
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/profiles":
		_ = json.NewEncoder(w).Encode(f.profiles)
	case "POST /api/device-readings/lookup":
		f.lookups++
		var req struct {
			ExternalIDs []string `json:"externalIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := []ltReadingRef{}
		for _, id := range req.ExternalIDs {
			if rd, ok := f.imported[id]; ok {
				out = append(out, ltReadingRef{ExternalID: id, ReportID: "r-" + id, ProfileID: rd.ProfileID})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"imported": out})
	case "POST /api/device-readings":
		var rd ltReading
		if err := json.NewDecoder(r.Body).Decode(&rd); err != nil {
			f.t.Errorf("bad import body: %v", err)
		}
		f.posts = append(f.posts, rd)
		status, code := "imported", http.StatusCreated
		if _, ok := f.imported[rd.ExternalID]; ok {
			status, code = "exists", http.StatusOK
		} else {
			f.imported[rd.ExternalID] = rd
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(ltImportResp{Status: status, ltReadingRef: ltReadingRef{ExternalID: rd.ExternalID, ProfileID: rd.ProfileID}})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// writeReadingsFile saves made-up records in the -format json layout.
func writeReadingsFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "readings.json")
	data := `[
 {"raw":"21 90 19 03 0e 08 1e 00 c3 00 6e 00 37 00 00 00 00 01 07 00 00"},
 {"raw":"21 90 19 03 0e 08 15 00 96 00 3e 00 30 00 00 00 00 01 07 00 00"},
 {"raw":"21 90 19 02 02 13 2d 00 e7 00 b4 00 27 00 00 00 00 01 1f 00 00"}
]`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// useLabTracker points the importer at srv with the given token and no config file.
func useLabTracker(t *testing.T, url, token string) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LAB_TRACKER_URL", url)
	t.Setenv("LAB_TRACKER_TOKEN", token)
}

func runImportWith(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	var prompts strings.Builder
	err := runImport(args, strings.NewReader(input), &prompts)
	return prompts.String(), err
}

func TestImport_AssignsProfilesAndSkipsKnown(t *testing.T) {
	lt, srv := newFakeLabTracker(t)
	useLabTracker(t, srv.URL, testToken)
	file := writeReadingsFile(t)

	// Run 1: Sam for the first, Enter (= Sam again) for the second, skip the third.
	if _, err := runImportWith(t, "2\n\ns\n", "-in", file); err != nil {
		t.Fatal(err)
	}
	if len(lt.posts) != 2 {
		t.Fatalf("posted %d readings, want 2", len(lt.posts))
	}
	for _, p := range lt.posts {
		if p.ProfileID != "p-sam" || p.Device != "CURO L7" || len(p.Results) != 3 {
			t.Errorf("posted %+v", p)
		}
	}
	first := lt.posts[0]
	if first.TakenAt[:16] != "2025-03-14T20:30" || first.Results[0] != (ltResult{"Total Cholesterol", 195, "mg/dL"}) {
		t.Errorf("first reading %+v", first)
	}

	// Run 2: only the skipped reading is new, and it goes to Alex.
	prompts, err := runImportWith(t, "1\n", "-in", file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(prompts, "TC ") != 1 {
		t.Errorf("asked about %d readings, want 1:\n%s", strings.Count(prompts, "TC "), prompts)
	}
	if len(lt.posts) != 3 || lt.posts[2].ProfileID != "p-alex" || !strings.Contains(prompts, "TC 231") {
		t.Errorf("third post %+v", lt.posts[len(lt.posts)-1])
	}

	// Run 3: nothing new, no prompts, no posts.
	prompts, err = runImportWith(t, "", "-in", file)
	if err != nil || prompts != "" || len(lt.posts) != 3 {
		t.Errorf("err=%v prompts=%q posts=%d", err, prompts, len(lt.posts))
	}
}

func TestImport_QuitStopsEarly(t *testing.T) {
	lt, srv := newFakeLabTracker(t)
	useLabTracker(t, srv.URL, testToken)
	if _, err := runImportWith(t, "1\nq\n", "-in", writeReadingsFile(t)); err != nil {
		t.Fatal(err)
	}
	if len(lt.posts) != 1 {
		t.Errorf("posted %d readings, want 1", len(lt.posts))
	}
}

// A bad token fails on the first request, before any reading is looked at.
func TestImport_BadTokenFailsFirst(t *testing.T) {
	lt, srv := newFakeLabTracker(t)
	useLabTracker(t, srv.URL, "lt_wrong")
	_, err := runImportWith(t, "", "-in", filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil || !strings.Contains(err.Error(), "invalid token") {
		t.Fatalf("err = %v", err)
	}
	if lt.lookups != 0 {
		t.Error("looked up readings despite a bad token")
	}
}

func TestImport_MissingConfig(t *testing.T) {
	useLabTracker(t, "", "")
	if _, err := runImportWith(t, "", "-in", writeReadingsFile(t)); err == nil || !strings.Contains(err.Error(), "LAB_TRACKER_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}
