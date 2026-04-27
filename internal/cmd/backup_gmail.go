package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/gmail/v1"

	"github.com/steipete/gogcli/internal/backup"
)

type gmailBackupOptions struct {
	Query            string
	Max              int64
	IncludeSpamTrash bool
	ShardMaxRows     int
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
	labels, err := fetchGmailBackupLabels(ctx, svc)
	if err != nil {
		return backup.Snapshot{}, err
	}
	messages, err := fetchGmailBackupMessages(ctx, svc, opts)
	if err != nil {
		return backup.Snapshot{}, err
	}
	shards := make([]backup.PlainShard, 0, 1)
	labelShard, err := backup.NewJSONLShard(backupServiceGmail, "labels", accountHash, fmt.Sprintf("data/gmail/%s/labels.jsonl.gz.age", accountHash), labels)
	if err != nil {
		return backup.Snapshot{}, err
	}
	shards = append(shards, labelShard)
	messageShards, err := buildGmailMessageShards(accountHash, messages, opts.ShardMaxRows)
	if err != nil {
		return backup.Snapshot{}, err
	}
	shards = append(shards, messageShards...)
	return backup.Snapshot{
		Services: []string{backupServiceGmail},
		Accounts: []string{accountHash},
		Counts: map[string]int{
			"gmail.labels":   len(labels),
			"gmail.messages": len(messages),
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
	const maxConcurrency = 8
	sem := make(chan struct{}, maxConcurrency)
	type result struct {
		index int
		msg   gmailBackupMessage
		err   error
	}
	results := make(chan result, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(index int, messageID string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- result{index: index, err: ctx.Err()}
				return
			}
			msg, err := svc.Users.Messages.Get("me", messageID).
				Format(gmailFormatRaw).
				Fields("id,threadId,historyId,internalDate,labelIds,sizeEstimate,raw").
				Context(ctx).
				Do()
			if err != nil {
				results <- result{index: index, err: fmt.Errorf("gmail message %s: %w", messageID, err)}
				return
			}
			if strings.TrimSpace(msg.Raw) == "" {
				results <- result{index: index, err: fmt.Errorf("gmail message %s returned empty raw payload", messageID)}
				return
			}
			results <- result{index: index, msg: gmailBackupMessage{
				ID:           msg.Id,
				ThreadID:     msg.ThreadId,
				HistoryID:    formatHistoryID(msg.HistoryId),
				InternalDate: msg.InternalDate,
				LabelIDs:     append([]string(nil), msg.LabelIds...),
				SizeEstimate: msg.SizeEstimate,
				Raw:          msg.Raw,
			}}
		}(i, id)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	ordered := make([]gmailBackupMessage, len(ids))
	var firstErr error
	for res := range results {
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		ordered[res.index] = res.msg
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return ordered, nil
}

func listGmailBackupMessageIDs(ctx context.Context, svc *gmail.Service, opts gmailBackupOptions) ([]string, error) {
	var ids []string
	pageToken := ""
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
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return ids, nil
}

func buildGmailMessageShards(accountHash string, messages []gmailBackupMessage, shardMaxRows int) ([]backup.PlainShard, error) {
	if shardMaxRows <= 0 {
		shardMaxRows = 1000
	}
	buckets := map[string][]gmailBackupMessage{}
	for _, message := range messages {
		t := time.UnixMilli(message.InternalDate).UTC()
		if message.InternalDate <= 0 {
			t = time.Unix(0, 0).UTC()
		}
		key := fmt.Sprintf("%04d/%02d", t.Year(), int(t.Month()))
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
