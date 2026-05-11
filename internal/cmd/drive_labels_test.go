package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/api/drivelabels/v2"
)

func TestDriveLabelsList_JSON(t *testing.T) {
	svc, closeSvc := newGoogleTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/labels" {
			http.NotFound(w, r)
			return
		}
		requireQuery(t, r, "publishedOnly", "true")
		requireQuery(t, r, "view", "LABEL_VIEW_BASIC")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"labels": []map[string]any{{
				"name":       "labels/abc",
				"id":         "abc",
				"revisionId": "1",
				"labelType":  "SHARED",
				"properties": map[string]any{"title": "Project"},
				"lifecycle":  map[string]any{"state": "PUBLISHED"},
			}},
		})
	}), drivelabels.NewService)
	defer closeSvc()
	orig := newDriveLabelsService
	t.Cleanup(func() { newDriveLabelsService = orig })
	newDriveLabelsService = func(context.Context, string) (*drivelabels.Service, error) { return svc, nil }

	out := captureStdout(t, func() {
		if err := (&DriveLabelsListCmd{PublishedOnly: true, View: "LABEL_VIEW_BASIC"}).Run(newCalendarJSONContext(t), &RootFlags{Account: "a@example.com"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	var parsed struct {
		LabelCount int `json:"labelCount"`
		Labels     []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("json parse: %v\n%s", err, out)
	}
	if parsed.LabelCount != 1 || len(parsed.Labels) != 1 || parsed.Labels[0].Name != "labels/abc" {
		t.Fatalf("unexpected output: %#v", parsed)
	}
}

func TestNormalizeDriveLabelName(t *testing.T) {
	if got := normalizeDriveLabelName("abc"); got != "labels/abc" {
		t.Fatalf("unexpected: %q", got)
	}
	if got := normalizeDriveLabelName("labels/abc"); got != "labels/abc" {
		t.Fatalf("unexpected: %q", got)
	}
}

func TestWrapDriveLabelsErrorValidCustomer(t *testing.T) {
	err := wrapDriveLabelsError(errors.New("Cannot perform this action without a valid customer"))
	if err == nil || !strings.Contains(err.Error(), "requires a Google Workspace customer") {
		t.Fatalf("unexpected error: %v", err)
	}
}
