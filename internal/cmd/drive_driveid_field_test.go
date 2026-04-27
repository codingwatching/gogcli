package cmd

import (
	"strings"
	"testing"
)

func TestDriveFileListFieldsIncludesDriveID(t *testing.T) {
	if !strings.Contains(driveFileListFields, "driveId") {
		t.Fatalf("driveFileListFields must include driveId; got %q", driveFileListFields)
	}
}
