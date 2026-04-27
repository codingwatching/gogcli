package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"github.com/steipete/gogcli/internal/backup"
)

func TestBackupAccountHashStableAndOpaque(t *testing.T) {
	got := backupAccountHash("  User@Example.COM ")
	want := backupAccountHash("user@example.com")
	if got != want {
		t.Fatalf("hash not normalized: got %s want %s", got, want)
	}
	if len(got) != 24 {
		t.Fatalf("hash length = %d, want 24 hex chars", len(got))
	}
	if strings.Contains(got, "user") || strings.Contains(got, "example") {
		t.Fatalf("hash leaks account text: %s", got)
	}
}

func TestBuildGmailMessageShardsBucketsSortsAndChunks(t *testing.T) {
	accountHash := "accthash"
	messages := []gmailBackupMessage{
		{ID: "march-new", InternalDate: mustUnixMilli(t, "2026-03-02T10:00:00Z"), Raw: "raw-3"},
		{ID: "april-later", InternalDate: mustUnixMilli(t, "2026-04-02T10:00:00Z"), Raw: "raw-2"},
		{ID: "april-earlier-b", InternalDate: mustUnixMilli(t, "2026-04-01T10:00:00Z"), Raw: "raw-1b"},
		{ID: "april-earlier-a", InternalDate: mustUnixMilli(t, "2026-04-01T10:00:00Z"), Raw: "raw-1a"},
	}

	shards, err := buildGmailMessageShards(accountHash, messages, 2)
	if err != nil {
		t.Fatalf("buildGmailMessageShards: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("len(shards) = %d, want 3", len(shards))
	}
	wantPaths := []string{
		"data/gmail/accthash/messages/2026/03/part-0001.jsonl.gz.age",
		"data/gmail/accthash/messages/2026/04/part-0001.jsonl.gz.age",
		"data/gmail/accthash/messages/2026/04/part-0002.jsonl.gz.age",
	}
	for i, want := range wantPaths {
		if shards[i].Path != want {
			t.Fatalf("shards[%d].Path = %q, want %q", i, shards[i].Path, want)
		}
	}
	if shards[0].Rows != 1 || shards[1].Rows != 2 || shards[2].Rows != 1 {
		t.Fatalf("unexpected row counts: %d %d %d", shards[0].Rows, shards[1].Rows, shards[2].Rows)
	}

	var aprilFirst []gmailBackupMessage
	if err := backup.DecodeJSONL(shards[1].Plaintext, &aprilFirst); err != nil {
		t.Fatalf("DecodeJSONL: %v", err)
	}
	gotIDs := []string{aprilFirst[0].ID, aprilFirst[1].ID}
	wantIDs := []string{"april-earlier-a", "april-earlier-b"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("april shard IDs = %v, want %v", gotIDs, wantIDs)
		}
	}
}

func TestMergeBackupSnapshotsKeepsCountsAndShardOrder(t *testing.T) {
	left := backup.Snapshot{
		Services: []string{"gmail"},
		Accounts: []string{"acct1"},
		Counts:   map[string]int{"gmail.messages": 2},
		Shards:   []backup.PlainShard{{Path: "data/gmail/acct1/messages/2026/04/part-0001.jsonl.gz.age"}},
	}
	right := backup.Snapshot{
		Services: []string{"calendar"},
		Accounts: []string{"acct1"},
		Counts:   map[string]int{"calendar.events": 3},
		Shards:   []backup.PlainShard{{Path: "data/calendar/acct1/events.jsonl.gz.age"}},
	}

	merged := mergeBackupSnapshots(left, right)
	if merged.Counts["gmail.messages"] != 2 || merged.Counts["calendar.events"] != 3 {
		t.Fatalf("unexpected counts: %+v", merged.Counts)
	}
	if len(merged.Shards) != 2 || merged.Shards[0].Path != left.Shards[0].Path || merged.Shards[1].Path != right.Shards[0].Path {
		t.Fatalf("unexpected shard order: %+v", merged.Shards)
	}
}

func TestExpandBackupServicesAllIncludesWorkspaceAdapters(t *testing.T) {
	got := strings.Join(expandBackupServices([]string{"all"}), ",")
	for _, want := range []string{
		"appscript",
		"calendar",
		"chat",
		"classroom",
		"contacts",
		"drive",
		"gmail",
		"gmail-settings",
		"groups",
		"admin",
		"keep",
		"tasks",
		"workspace",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expanded all missing %q in %q", want, got)
		}
	}
}

func TestGmailBackupMessageCacheRoundTrips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	message := gmailBackupMessage{
		ID:           "msg-one",
		ThreadID:     "thread-one",
		InternalDate: mustUnixMilli(t, "2026-04-02T10:00:00Z"),
		LabelIDs:     []string{"INBOX"},
		Raw:          base64.RawURLEncoding.EncodeToString([]byte("Subject: Cached\r\n\r\nBody")),
	}
	if err := writeGmailBackupMessageCache("accthash", message); err != nil {
		t.Fatalf("writeGmailBackupMessageCache: %v", err)
	}
	got, ok, err := readGmailBackupMessageCache("accthash", "msg-one")
	if err != nil {
		t.Fatalf("readGmailBackupMessageCache: %v", err)
	}
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.ID != message.ID || got.ThreadID != message.ThreadID || got.Raw != message.Raw {
		t.Fatalf("cache round trip mismatch: %#v", got)
	}
	path, ok := gmailBackupMessageCachePath("accthash", "msg-one")
	if !ok {
		t.Fatal("expected cache path")
	}
	if strings.Contains(path, "msg-one") {
		t.Fatalf("cache path should hash message IDs, got %q", path)
	}
}

func TestGmailBackupMessageCacheRejectsWrongID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	message := gmailBackupMessage{ID: "msg-one", Raw: "raw"}
	if err := writeGmailBackupMessageCache("accthash", message); err != nil {
		t.Fatalf("writeGmailBackupMessageCache: %v", err)
	}
	path, ok := gmailBackupMessageCachePath("accthash", "msg-one")
	if !ok {
		t.Fatal("expected cache path")
	}
	data, err := json.Marshal(gmailBackupMessage{ID: "other", Raw: "raw"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := readGmailBackupMessageCache("accthash", "msg-one"); err == nil {
		t.Fatal("expected wrong cache ID to fail")
	}
}

func TestFetchBackupDriveCollaborationCollectsMetadataAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/file1/permissions"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"permissions": []map[string]any{{"id": "perm1", "type": "user", "role": "reader", "emailAddress": "a@example.com"}},
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/file1/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"comments": []map[string]any{{"id": "comment1", "content": "hello", "resolved": false}},
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/file1/revisions"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"revisions": []map[string]any{{"id": "rev1", "modifiedTime": "2026-04-02T10:00:00Z"}},
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/file2/permissions"):
			http.Error(w, `{"error":{"message":"denied"}}`, http.StatusForbidden)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/file2/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/files/file2/revisions"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc, err := drive.NewService(t.Context(),
		option.WithoutAuthentication(),
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL+"/"),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	got, counts := fetchBackupDriveCollaboration(t.Context(), svc, []driveBackupFile{
		{File: &drive.File{Id: "file1"}},
		{File: &drive.File{Id: "file2"}},
	})
	if counts["drive.permissions"] != 2 || counts["drive.comments"] != 1 || counts["drive.revisions"] != 1 || counts["drive.collab.errors"] != 1 {
		t.Fatalf("unexpected counts: %#v", counts)
	}
	if got.Permissions[0].FileID != "file1" || got.Permissions[0].Permission.Id != "perm1" {
		t.Fatalf("unexpected permission row: %#v", got.Permissions[0])
	}
	if got.Permissions[1].FileID != "file2" || got.Permissions[1].Error == "" {
		t.Fatalf("expected file2 permission error row: %#v", got.Permissions[1])
	}
}

func TestDomainFromAccount(t *testing.T) {
	if got := domainFromAccount("Admin@Example.COM"); got != "Example.COM" {
		t.Fatalf("domainFromAccount = %q", got)
	}
	if got := domainFromAccount("example.com"); got != "example.com" {
		t.Fatalf("domainFromAccount without @ = %q", got)
	}
}

func TestDriveBackupContentPlansPreferReadableWorkspaceFormats(t *testing.T) {
	docPlans := driveBackupContentPlans(&drive.File{Id: "doc1", Name: "Spec", MimeType: driveMimeGoogleDoc}, false)
	if len(docPlans) != 2 || docPlans[0].Extension != ".docx" || docPlans[1].Extension != ".md" {
		t.Fatalf("unexpected doc plans: %#v", docPlans)
	}
	sheetPlans := driveBackupContentPlans(&drive.File{Id: "sheet1", Name: "Budget", MimeType: driveMimeGoogleSheet}, false)
	if len(sheetPlans) != 1 || sheetPlans[0].Extension != ".xlsx" {
		t.Fatalf("unexpected sheet plans: %#v", sheetPlans)
	}
	folderPlans := driveBackupContentPlans(&drive.File{Id: "folder1", Name: "Folder", MimeType: driveMimeGoogleFolder}, false)
	if len(folderPlans) != 0 {
		t.Fatalf("folder should not have content plans: %#v", folderPlans)
	}
	binaryPlans := driveBackupContentPlans(&drive.File{Id: "bin1", Name: "Archive.zip", MimeType: "application/zip"}, false)
	if len(binaryPlans) != 0 {
		t.Fatalf("binary should be opt-in: %#v", binaryPlans)
	}
	binaryPlans = driveBackupContentPlans(&drive.File{Id: "bin1", Name: "Archive.zip", MimeType: "application/zip"}, true)
	if len(binaryPlans) != 1 || binaryPlans[0].Source != "download" {
		t.Fatalf("unexpected binary plans: %#v", binaryPlans)
	}
}

func TestDownloadDriveBackupContentHonorsTimeout(t *testing.T) {
	origExport := driveExportDownload
	t.Cleanup(func() { driveExportDownload = origExport })
	driveExportDownload = func(ctx context.Context, _ *drive.Service, _, _ string) (*http.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := downloadDriveBackupContent(t.Context(), nil, &drive.File{Id: "doc1"}, driveBackupContentPlan{
		MimeType: mimePDF,
		Source:   "export",
	}, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestDecodeGmailRawAcceptsBase64URLVariants(t *testing.T) {
	payload := []byte("Subject: Hello\r\n\r\nBody")
	raw := base64.RawURLEncoding.EncodeToString(payload)
	got, err := decodeGmailRaw(raw)
	if err != nil {
		t.Fatalf("decodeGmailRaw raw: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("raw decoded = %q, want %q", got, payload)
	}

	padded := base64.URLEncoding.EncodeToString(payload)
	got, err = decodeGmailRaw(padded)
	if err != nil {
		t.Fatalf("decodeGmailRaw padded: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("padded decoded = %q, want %q", got, payload)
	}
}

func TestExportGmailMessagesWritesReadableEMLAndIndex(t *testing.T) {
	outDir := t.TempDir()
	payload := []byte("Subject: Hello\r\nFrom: a@example.com\r\n\r\nBody")
	message := gmailBackupMessage{
		ID:           "msg/one",
		ThreadID:     "thread-1",
		InternalDate: mustUnixMilli(t, "2026-04-02T10:00:00Z"),
		LabelIDs:     []string{"INBOX"},
		Raw:          base64.RawURLEncoding.EncodeToString(payload),
	}
	shard, err := backup.NewJSONLShard("gmail", "messages", "acct/hash", "data/gmail/acct/messages/2026/04/part-0001.jsonl.gz.age", []gmailBackupMessage{message})
	if err != nil {
		t.Fatalf("NewJSONLShard: %v", err)
	}

	files, count, err := exportGmailMessages(outDir, shard)
	if err != nil {
		t.Fatalf("exportGmailMessages: %v", err)
	}
	if files != 2 || count != 1 {
		t.Fatalf("files,count = %d,%d want 2,1", files, count)
	}

	emlRel := backupExportMessagePath("acct_hash", message)
	eml, err := os.ReadFile(filepath.Join(outDir, filepath.FromSlash(emlRel)))
	if err != nil {
		t.Fatalf("read eml: %v", err)
	}
	if string(eml) != string(payload) {
		t.Fatalf("eml = %q, want %q", eml, payload)
	}
	index := readText(t, filepath.Join(outDir, "gmail", "acct_hash", "messages", "index.jsonl"))
	if !strings.Contains(index, `"id":"msg/one"`) || !strings.Contains(index, `"eml":"`+emlRel+`"`) {
		t.Fatalf("index missing expected fields: %s", index)
	}
}

func TestExportDriveContentsWritesReadableFilesAndIndex(t *testing.T) {
	outDir := t.TempDir()
	row := driveBackupContent{
		FileID:     "file/one",
		Name:       "Quarterly Plan",
		MimeType:   driveMimeGoogleDoc,
		ExportName: "Quarterly_Plan.md",
		ExportMime: mimeTextMarkdown,
		Source:     "export",
		Size:       8,
		DataBase64: base64.StdEncoding.EncodeToString([]byte("# Plan\n")),
	}
	shard, err := backup.NewJSONLShard("drive", "contents", "acct/hash", "data/drive/acct/contents/part-0001.jsonl.gz.age", []driveBackupContent{row})
	if err != nil {
		t.Fatalf("NewJSONLShard: %v", err)
	}

	files, count, err := exportDriveContents(outDir, shard)
	if err != nil {
		t.Fatalf("exportDriveContents: %v", err)
	}
	if files != 2 || count != 1 {
		t.Fatalf("files,count = %d,%d want 2,1", files, count)
	}
	exported := readText(t, filepath.Join(outDir, "drive", "acct_hash", "files", "file_one", "Quarterly_Plan.md"))
	if exported != "# Plan\n" {
		t.Fatalf("exported = %q", exported)
	}
	index := readText(t, filepath.Join(outDir, "drive", "acct_hash", "files", "index.jsonl"))
	if !strings.Contains(index, `"fileId":"file/one"`) || !strings.Contains(index, `"path":"drive/acct_hash/files/file_one/Quarterly_Plan.md"`) {
		t.Fatalf("index missing expected fields: %s", index)
	}
}

func TestEnsureExportOutsideRepoRejectsNestedPlaintext(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "data"), 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := ensureExportOutsideRepo(filepath.Join(repo, "plaintext"), repo); err == nil {
		t.Fatal("expected nested export dir to be rejected")
	}
	if err := ensureExportOutsideRepo(filepath.Join(filepath.Dir(repo), "export"), repo); err != nil {
		t.Fatalf("outside export rejected: %v", err)
	}
}

func mustUnixMilli(t *testing.T, value string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed.UnixMilli()
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
