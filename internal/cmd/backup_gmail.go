package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/gmail/v1"

	"github.com/steipete/gogcli/internal/backup"
	"github.com/steipete/gogcli/internal/ui"
)

type gmailBackupOptions struct {
	Query            string
	Max              int64
	IncludeSpamTrash bool
	ShardMaxRows     int
	AccountHash      string
	CacheMessages    bool
	RefreshCache     bool
}

type gmailBackupMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId,omitempty"`
	HistoryID    string   `json:"historyId,omitempty"`
	InternalDate int64    `json:"internalDate,omitempty"`
	LabelIDs     []string `json:"labelIds,omitempty"`
	SizeEstimate int64    `json:"sizeEstimate,omitempty"`
	Raw          string   `json:"raw"`
}

type gmailBackupLabel struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type,omitempty"`
	MessageListVisibility string `json:"messageListVisibility,omitempty"`
	LabelListVisibility   string `json:"labelListVisibility,omitempty"`
	MessagesTotal         int64  `json:"messagesTotal,omitempty"`
	MessagesUnread        int64  `json:"messagesUnread,omitempty"`
	ThreadsTotal          int64  `json:"threadsTotal,omitempty"`
	ThreadsUnread         int64  `json:"threadsUnread,omitempty"`
}

type gmailBackupListState struct {
	Version          int       `json:"version"`
	AccountHash      string    `json:"accountHash"`
	Query            string    `json:"query,omitempty"`
	Max              int64     `json:"max,omitempty"`
	IncludeSpamTrash bool      `json:"includeSpamTrash"`
	PageToken        string    `json:"pageToken,omitempty"`
	IDs              []string  `json:"ids"`
	Complete         bool      `json:"complete"`
	Updated          time.Time `json:"updated"`
}

func buildGmailBackupSnapshot(ctx context.Context, flags *RootFlags, opts gmailBackupOptions) (backup.Snapshot, error) {
	if opts.ShardMaxRows <= 0 {
		opts.ShardMaxRows = 1000
	}
	account, err := requireAccount(flags)
	if err != nil {
		return backup.Snapshot{}, err
	}
	svc, err := newGmailService(ctx, account)
	if err != nil {
		return backup.Snapshot{}, err
	}
	accountHash := backupAccountHash(account)
	opts.AccountHash = accountHash
	labels, err := fetchGmailBackupLabels(ctx, svc)
	if err != nil {
		return backup.Snapshot{}, err
	}
	shards := make([]backup.PlainShard, 0, 1)
	labelShard, err := backup.NewJSONLShard(backupServiceGmail, "labels", accountHash, fmt.Sprintf("data/gmail/%s/labels.jsonl.gz.age", accountHash), labels)
	if err != nil {
		return backup.Snapshot{}, err
	}
	shards = append(shards, labelShard)
	var messageCount int
	if opts.CacheMessages {
		ids, listErr := listGmailBackupMessageIDs(ctx, svc, opts)
		if listErr != nil {
			return backup.Snapshot{}, listErr
		}
		if cacheErr := ensureGmailBackupMessageCache(ctx, svc, opts, ids); cacheErr != nil {
			return backup.Snapshot{}, cacheErr
		}
		messageShards, shardErr := buildGmailMessageShardsFromCache(ctx, opts, ids)
		if shardErr != nil {
			return backup.Snapshot{}, shardErr
		}
		shards = append(shards, messageShards...)
		messageCount = len(ids)
	} else {
		messages, err := fetchGmailBackupMessages(ctx, svc, opts)
		if err != nil {
			return backup.Snapshot{}, err
		}
		messageShards, err := buildGmailMessageShards(accountHash, messages, opts.ShardMaxRows)
		if err != nil {
			return backup.Snapshot{}, err
		}
		shards = append(shards, messageShards...)
		messageCount = len(messages)
	}
	return backup.Snapshot{
		Services: []string{backupServiceGmail},
		Accounts: []string{accountHash},
		Counts: map[string]int{
			"gmail.labels":   len(labels),
			"gmail.messages": messageCount,
		},
		Shards: shards,
	}, nil
}

func fetchGmailBackupLabels(ctx context.Context, svc *gmail.Service) ([]gmailBackupLabel, error) {
	resp, err := svc.Users.Labels.List("me").Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	out := make([]gmailBackupLabel, 0, len(resp.Labels))
	for _, label := range resp.Labels {
		if label == nil {
			continue
		}
		out = append(out, gmailBackupLabel{
			ID:                    label.Id,
			Name:                  label.Name,
			Type:                  label.Type,
			MessageListVisibility: label.MessageListVisibility,
			LabelListVisibility:   label.LabelListVisibility,
			MessagesTotal:         label.MessagesTotal,
			MessagesUnread:        label.MessagesUnread,
			ThreadsTotal:          label.ThreadsTotal,
			ThreadsUnread:         label.ThreadsUnread,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func fetchGmailBackupMessages(ctx context.Context, svc *gmail.Service, opts gmailBackupOptions) ([]gmailBackupMessage, error) {
	ids, err := listGmailBackupMessageIDs(ctx, svc, opts)
	if err != nil {
		return nil, err
	}
	if !opts.CacheMessages {
		return fetchGmailBackupMessagesDirect(ctx, svc, ids)
	}
	if err := ensureGmailBackupMessageCache(ctx, svc, opts, ids); err != nil {
		return nil, err
	}
	ordered := make([]gmailBackupMessage, 0, len(ids))
	for _, id := range ids {
		msg, ok, err := readGmailBackupMessageCache(opts.AccountHash, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("gmail message %s missing from backup cache", id)
		}
		ordered = append(ordered, msg)
	}
	return ordered, nil
}

func fetchGmailBackupMessagesDirect(ctx context.Context, svc *gmail.Service, ids []string) ([]gmailBackupMessage, error) {
	gmailBackupProgressf(ctx, "backup gmail fetch\tqueued=%d", len(ids))
	out := make([]gmailBackupMessage, 0, len(ids))
	for i, id := range ids {
		msg, err := svc.Users.Messages.Get("me", id).
			Format(gmailFormatRaw).
			Fields("id,threadId,historyId,internalDate,labelIds,sizeEstimate,raw").
			Context(ctx).
			Do()
		if err != nil {
			return nil, fmt.Errorf("gmail message %s: %w", id, err)
		}
		if strings.TrimSpace(msg.Raw) == "" {
			return nil, fmt.Errorf("gmail message %s returned empty raw payload", id)
		}
		out = append(out, gmailBackupMessage{
			ID:           msg.Id,
			ThreadID:     msg.ThreadId,
			HistoryID:    formatHistoryID(msg.HistoryId),
			InternalDate: msg.InternalDate,
			LabelIDs:     append([]string(nil), msg.LabelIds...),
			SizeEstimate: msg.SizeEstimate,
			Raw:          msg.Raw,
		})
		done := i + 1
		if done == len(ids) || done%100 == 0 {
			gmailBackupProgressf(ctx, "backup gmail fetch\t%d/%d\tfetched=%d\tcache=0", done, len(ids), done)
		}
	}
	return out, nil
}

func ensureGmailBackupMessageCache(ctx context.Context, svc *gmail.Service, opts gmailBackupOptions, ids []string) error {
	gmailBackupProgressf(ctx, "backup gmail fetch\tqueued=%d", len(ids))
	const maxConcurrency = 2
	sem := make(chan struct{}, maxConcurrency)
	type result struct {
		cache bool
		err   error
	}
	results := make(chan result, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(messageID string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- result{err: ctx.Err()}
				return
			}
			if opts.CacheMessages && !opts.RefreshCache {
				_, ok, err := readGmailBackupMessageCache(opts.AccountHash, messageID)
				if err != nil {
					results <- result{err: err}
					return
				}
				if ok {
					results <- result{cache: true}
					return
				}
			}
			msg, err := svc.Users.Messages.Get("me", messageID).
				Format(gmailFormatRaw).
				Fields("id,threadId,historyId,internalDate,labelIds,sizeEstimate,raw").
				Context(ctx).
				Do()
			if err != nil {
				results <- result{err: fmt.Errorf("gmail message %s: %w", messageID, err)}
				return
			}
			if strings.TrimSpace(msg.Raw) == "" {
				results <- result{err: fmt.Errorf("gmail message %s returned empty raw payload", messageID)}
				return
			}
			backupMsg := gmailBackupMessage{
				ID:           msg.Id,
				ThreadID:     msg.ThreadId,
				HistoryID:    formatHistoryID(msg.HistoryId),
				InternalDate: msg.InternalDate,
				LabelIDs:     append([]string(nil), msg.LabelIds...),
				SizeEstimate: msg.SizeEstimate,
				Raw:          msg.Raw,
			}
			if opts.CacheMessages {
				if err := writeGmailBackupMessageCache(opts.AccountHash, backupMsg); err != nil {
					results <- result{err: err}
					return
				}
			}
			results <- result{}
		}(id)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	var firstErr error
	done := 0
	cacheHits := 0
	fetched := 0
	for res := range results {
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		done++
		if res.cache {
			cacheHits++
		} else if res.err == nil {
			fetched++
		}
		if done == len(ids) || done%100 == 0 {
			gmailBackupProgressf(ctx, "backup gmail fetch\t%d/%d\tfetched=%d\tcache=%d", done, len(ids), fetched, cacheHits)
		}
		if done%1000 == 0 {
			debug.FreeOSMemory()
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func readGmailBackupMessageCache(accountHash, messageID string) (gmailBackupMessage, bool, error) {
	path, ok := gmailBackupMessageCachePath(accountHash, messageID)
	if !ok {
		return gmailBackupMessage{}, false, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // cache path is derived from the OS cache dir, account hash, and hashed message ID.
	if err != nil {
		if os.IsNotExist(err) {
			return gmailBackupMessage{}, false, nil
		}
		return gmailBackupMessage{}, false, fmt.Errorf("read gmail backup cache %s: %w", path, err)
	}
	var msg gmailBackupMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return gmailBackupMessage{}, false, fmt.Errorf("decode gmail backup cache %s: %w", path, err)
	}
	if msg.ID != messageID {
		return gmailBackupMessage{}, false, fmt.Errorf("gmail backup cache %s has id %q, want %q", path, msg.ID, messageID)
	}
	if strings.TrimSpace(msg.Raw) == "" {
		return gmailBackupMessage{}, false, fmt.Errorf("gmail backup cache %s has empty raw payload", path)
	}
	return msg, true, nil
}

func writeGmailBackupMessageCache(accountHash string, msg gmailBackupMessage) error {
	if strings.TrimSpace(msg.ID) == "" {
		return nil
	}
	path, ok := gmailBackupMessageCachePath(accountHash, msg.ID)
	if !ok {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create gmail backup cache dir: %w", err)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode gmail backup cache %s: %w", msg.ID, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".message-*.json")
	if err != nil {
		return fmt.Errorf("create gmail backup cache temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write gmail backup cache temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close gmail backup cache temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod gmail backup cache temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace gmail backup cache %s: %w", path, err)
	}
	return nil
}

func gmailBackupMessageCachePath(accountHash, messageID string) (string, bool) {
	accountHash = strings.TrimSpace(accountHash)
	messageID = strings.TrimSpace(messageID)
	if accountHash == "" || messageID == "" {
		return "", false
	}
	dir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(messageID))
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(dir, "gogcli", "backup", "gmail", accountHash, "raw-v1", name), true
}

func listGmailBackupMessageIDs(ctx context.Context, svc *gmail.Service, opts gmailBackupOptions) ([]string, error) {
	var ids []string
	pageToken := ""
	statePath, hasStatePath := gmailBackupListStatePath(opts)
	if opts.CacheMessages && !opts.RefreshCache && hasStatePath {
		state, ok, err := readGmailBackupListState(statePath)
		if err != nil {
			return nil, err
		}
		if ok {
			if state.Complete {
				gmailBackupProgressf(ctx, "backup gmail list\tresume=complete\tmessages=%d", len(state.IDs))
				return append([]string(nil), state.IDs...), nil
			}
			ids = append(ids, state.IDs...)
			pageToken = state.PageToken
			gmailBackupProgressf(ctx, "backup gmail list\tresume=partial\tmessages=%d", len(ids))
		}
	}
	gmailBackupProgressf(ctx, "backup gmail list\tstart\tmessages=%d", len(ids))
	for {
		maxResults := int64(500)
		if opts.Max > 0 {
			remaining := opts.Max - int64(len(ids))
			if remaining <= 0 {
				break
			}
			if remaining < maxResults {
				maxResults = remaining
			}
		}
		call := svc.Users.Messages.List("me").
			MaxResults(maxResults).
			IncludeSpamTrash(opts.IncludeSpamTrash).
			Fields("messages(id),nextPageToken").
			Context(ctx)
		if strings.TrimSpace(opts.Query) != "" {
			call = call.Q(opts.Query)
		}
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, err
		}
		for _, message := range resp.Messages {
			if message != nil && strings.TrimSpace(message.Id) != "" {
				ids = append(ids, message.Id)
			}
		}
		gmailBackupProgressf(ctx, "backup gmail list\tmessages=%d", len(ids))
		complete := resp.NextPageToken == "" || (opts.Max > 0 && int64(len(ids)) >= opts.Max)
		if complete {
			if opts.CacheMessages && hasStatePath {
				if err := writeGmailBackupListState(statePath, opts, ids, "", true); err != nil {
					return nil, err
				}
			}
			break
		}
		pageToken = resp.NextPageToken
		if opts.CacheMessages && hasStatePath {
			if err := writeGmailBackupListState(statePath, opts, ids, pageToken, false); err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}

func readGmailBackupListState(path string) (gmailBackupListState, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from the OS cache dir and query hash.
	if err != nil {
		if os.IsNotExist(err) {
			return gmailBackupListState{}, false, nil
		}
		return gmailBackupListState{}, false, fmt.Errorf("read gmail backup list state %s: %w", path, err)
	}
	var state gmailBackupListState
	if err := json.Unmarshal(data, &state); err != nil {
		return gmailBackupListState{}, false, fmt.Errorf("decode gmail backup list state %s: %w", path, err)
	}
	if state.Version != 1 {
		return gmailBackupListState{}, false, nil
	}
	return state, true, nil
}

func writeGmailBackupListState(path string, opts gmailBackupOptions, ids []string, pageToken string, complete bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create gmail backup list state dir: %w", err)
	}
	state := gmailBackupListState{
		Version:          1,
		AccountHash:      opts.AccountHash,
		Query:            strings.TrimSpace(opts.Query),
		Max:              opts.Max,
		IncludeSpamTrash: opts.IncludeSpamTrash,
		PageToken:        pageToken,
		IDs:              append([]string(nil), ids...),
		Complete:         complete,
		Updated:          time.Now().UTC(),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode gmail backup list state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".list-*.json")
	if err != nil {
		return fmt.Errorf("create gmail backup list state temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write gmail backup list state temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close gmail backup list state temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod gmail backup list state temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace gmail backup list state %s: %w", path, err)
	}
	return nil
}

func gmailBackupListStatePath(opts gmailBackupOptions) (string, bool) {
	accountHash := strings.TrimSpace(opts.AccountHash)
	if accountHash == "" {
		return "", false
	}
	dir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return "", false
	}
	key := struct {
		Query            string `json:"query,omitempty"`
		Max              int64  `json:"max,omitempty"`
		IncludeSpamTrash bool   `json:"includeSpamTrash"`
	}{
		Query:            strings.TrimSpace(opts.Query),
		Max:              opts.Max,
		IncludeSpamTrash: opts.IncludeSpamTrash,
	}
	data, err := json.Marshal(key)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(dir, "gogcli", "backup", "gmail", accountHash, "list-v1", name), true
}

func gmailBackupProgressf(ctx context.Context, format string, args ...any) {
	u := ui.FromContext(ctx)
	if u == nil {
		return
	}
	u.Err().Printf(format, args...)
}

type gmailBackupMessageRef struct {
	ID           string
	InternalDate int64
}

func buildGmailMessageShardsFromCache(ctx context.Context, opts gmailBackupOptions, ids []string) ([]backup.PlainShard, error) {
	if opts.ShardMaxRows <= 0 {
		opts.ShardMaxRows = 1000
	}
	tempDir, ok := gmailBackupTempShardDir(opts.AccountHash)
	if !ok {
		return nil, fmt.Errorf("gmail backup temp shard directory unavailable")
	}
	if err := os.RemoveAll(tempDir); err != nil {
		return nil, fmt.Errorf("clear gmail backup temp shard dir: %w", err)
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create gmail backup temp shard dir: %w", err)
	}
	buckets := map[string][]gmailBackupMessageRef{}
	for i, id := range ids {
		msg, ok, err := readGmailBackupMessageCache(opts.AccountHash, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("gmail message %s missing from backup cache", id)
		}
		key := gmailBackupMessageMonthKey(msg.InternalDate)
		buckets[key] = append(buckets[key], gmailBackupMessageRef{ID: msg.ID, InternalDate: msg.InternalDate})
		done := i + 1
		if done == len(ids) || done%5000 == 0 {
			gmailBackupProgressf(ctx, "backup gmail shard-index\t%d/%d", done, len(ids))
		}
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	shards := make([]backup.PlainShard, 0, len(keys))
	shardCount := 0
	for _, key := range keys {
		refs := buckets[key]
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].InternalDate == refs[j].InternalDate {
				return refs[i].ID < refs[j].ID
			}
			return refs[i].InternalDate < refs[j].InternalDate
		})
		for part, start := 1, 0; start < len(refs); part, start = part+1, start+opts.ShardMaxRows {
			end := start + opts.ShardMaxRows
			if end > len(refs) {
				end = len(refs)
			}
			rel := fmt.Sprintf("data/gmail/%s/messages/%s/part-%04d.jsonl.gz.age", opts.AccountHash, key, part)
			shard, err := buildGmailMessageShardFromCache(opts.AccountHash, rel, tempDir, refs[start:end])
			if err != nil {
				return nil, err
			}
			shards = append(shards, shard)
			shardCount++
			if shardCount%25 == 0 || end == len(refs) {
				gmailBackupProgressf(ctx, "backup gmail shard-build\tshards=%d\tmessages=%d/%d", shardCount, countGmailShardRows(shards), len(ids))
			}
		}
	}
	return shards, nil
}

func buildGmailMessageShardFromCache(accountHash, rel, tempDir string, refs []gmailBackupMessageRef) (backup.PlainShard, error) {
	sum := sha256.Sum256([]byte(rel))
	path := filepath.Join(tempDir, hex.EncodeToString(sum[:])+".jsonl")
	f, err := os.Create(path) //nolint:gosec // path is derived from the OS cache dir and a hash of the shard path.
	if err != nil {
		return backup.PlainShard{}, fmt.Errorf("create gmail backup temp shard: %w", err)
	}
	enc := json.NewEncoder(f)
	count := 0
	for _, ref := range refs {
		msg, ok, err := readGmailBackupMessageCache(accountHash, ref.ID)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return backup.PlainShard{}, err
		}
		if !ok {
			_ = f.Close()
			_ = os.Remove(path)
			return backup.PlainShard{}, fmt.Errorf("gmail message %s missing from backup cache", ref.ID)
		}
		if err := enc.Encode(msg); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return backup.PlainShard{}, fmt.Errorf("encode gmail backup temp shard: %w", err)
		}
		count++
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return backup.PlainShard{}, fmt.Errorf("close gmail backup temp shard: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return backup.PlainShard{}, fmt.Errorf("chmod gmail backup temp shard: %w", err)
	}
	return backup.PlainShard{
		Service:       backupServiceGmail,
		Kind:          "messages",
		Account:       accountHash,
		Path:          filepath.ToSlash(rel),
		Rows:          count,
		PlaintextPath: path,
	}, nil
}

func countGmailShardRows(shards []backup.PlainShard) int {
	total := 0
	for _, shard := range shards {
		total += shard.Rows
	}
	return total
}

func gmailBackupTempShardDir(accountHash string) (string, bool) {
	accountHash = strings.TrimSpace(accountHash)
	if accountHash == "" {
		return "", false
	}
	dir, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return "", false
	}
	return filepath.Join(dir, "gogcli", "backup", "gmail", accountHash, "tmp-shards"), true
}

func gmailBackupMessageMonthKey(internalDate int64) string {
	t := time.UnixMilli(internalDate).UTC()
	if internalDate <= 0 {
		t = time.Unix(0, 0).UTC()
	}
	return fmt.Sprintf("%04d/%02d", t.Year(), int(t.Month()))
}

func buildGmailMessageShards(accountHash string, messages []gmailBackupMessage, shardMaxRows int) ([]backup.PlainShard, error) {
	if shardMaxRows <= 0 {
		shardMaxRows = 1000
	}
	buckets := map[string][]gmailBackupMessage{}
	for _, message := range messages {
		key := gmailBackupMessageMonthKey(message.InternalDate)
		buckets[key] = append(buckets[key], message)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	shards := make([]backup.PlainShard, 0, len(keys))
	for _, key := range keys {
		values := buckets[key]
		sort.Slice(values, func(i, j int) bool {
			if values[i].InternalDate == values[j].InternalDate {
				return values[i].ID < values[j].ID
			}
			return values[i].InternalDate < values[j].InternalDate
		})
		for part, start := 1, 0; start < len(values); part, start = part+1, start+shardMaxRows {
			end := start + shardMaxRows
			if end > len(values) {
				end = len(values)
			}
			rel := fmt.Sprintf("data/gmail/%s/messages/%s/part-%04d.jsonl.gz.age", accountHash, key, part)
			shard, err := backup.NewJSONLShard(backupServiceGmail, "messages", accountHash, rel, values[start:end])
			if err != nil {
				return nil, err
			}
			shards = append(shards, shard)
		}
	}
	return shards, nil
}
