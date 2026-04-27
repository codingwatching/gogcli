---
summary: "Encrypted Google account backups"
read_when:
  - Adding a new gog backup service adapter
  - Changing encrypted backup layout, manifest fields, or age identity handling
  - Debugging backup-gog push, status, or verify
---

# Encrypted Backups

`gog backup` writes Google account data into a Git repository as age-encrypted
JSONL gzip shards. The intended repository is private, for example
`https://github.com/steipete/backup-gog`, but service payloads are encrypted
before Git sees them.

## Commands

Initialize local config, create an age identity if needed, seed the backup repo,
and print the public recipient:

```bash
gog backup init \
  --repo ~/Projects/backup-gog \
  --remote https://github.com/steipete/backup-gog.git
```

Back up all supported services:

```bash
gog backup push --services all --account steipete@gmail.com
```

Back up only Gmail:

```bash
gog backup push --services gmail --account steipete@gmail.com
```

For a bounded smoke run:

```bash
gog backup push --services gmail --account steipete@gmail.com --query 'newer_than:7d' --max 25
```

Inspect cleartext manifest metadata:

```bash
gog backup status
```

Decrypt every shard and verify hashes and row counts:

```bash
gog backup verify
```

Decrypt one shard to stdout:

```bash
gog backup cat data/gmail/<account-hash>/labels.jsonl.gz.age --pretty
```

Write an unencrypted local copy for easy reading on the Mac:

```bash
gog backup export --out ~/Documents/gog-backup-export
```

Use `--no-push` on `init` or `push` to commit locally without pushing to the
remote.

Supported services:

- `gmail`: labels and raw MIME messages.
- `gmail-settings`: filters, forwarding addresses, auto-forwarding, send-as
  aliases, vacation responder, delegate visibility, POP, IMAP, and language
  settings.
- `calendar`: calendar list entries and all events, including deleted events.
- `contacts`: People API contacts and other contacts.
- `tasks`: task lists and tasks, including completed, deleted, hidden, and
  assigned tasks.
- `drive`: shared drives, Drive file metadata, and downloaded/exported file
  content. Google Docs export as `.docx` and Markdown, Sheets as `.xlsx`,
  Slides as `.pptx` and PDF, Drawings as PNG and PDF, and binary files as
  metadata-only unless `--drive-binary-contents` is set.
- `workspace`: Docs/Sheets/Slides inventory plus Forms and form responses
  discovered through Drive. Add `--workspace-native` to fetch full native
  Docs/Sheets/Slides API JSON.
- `appscript`: Apps Script projects and source content discovered through
  Drive.
- `chat`: Chat spaces and messages, when the authenticated account/API allows
  access.
- `classroom`: courses, topics, announcements, coursework, materials, and
  submissions visible to the authenticated account.

`all` expands to every supported service. Pushing a subset updates that subset
and preserves existing shards for services that were not selected, as long as
the age recipients are unchanged.

`gog backup push` enables `--drive-contents` and `--best-effort` by default.
Use `--no-drive-contents` for metadata-only Drive runs, or
`--drive-content-max-bytes <bytes>` to skip individual large Drive downloads.
Drive content exports Google-native files by default; set
`--drive-binary-contents` only when you intentionally want non-Google binary
file bytes in Git shards. Use `--workspace-native` only when you want the
heavier native API JSON in addition to readable Drive exports;
`--workspace-max-files` bounds that native fetch per file type for smoke tests.
Best-effort optional services record encrypted `errors` shards and let the rest
of the backup finish.

## Files

Local config:

```text
~/.gog/backup.json
~/.gog/age.key
```

Backup repo:

```text
README.md
manifest.json
data/gmail/<account-hash>/labels.jsonl.gz.age
data/gmail/<account-hash>/messages/YYYY/MM/part-0001.jsonl.gz.age
data/calendar/<account-hash>/...
data/contacts/<account-hash>/...
data/drive/<account-hash>/...
data/tasks/<account-hash>/...
```

`manifest.json` is intentionally cleartext. It contains format version, export
time, public age recipients, service names, account hashes, shard paths, row
counts, encrypted byte sizes, and plaintext hashes used for verification. It
does not contain email subjects, senders, recipients, bodies, raw message IDs,
or labels.

Plaintext export directory:

```text
README.md
manifest.json
gmail/<account-hash>/labels.json
gmail/<account-hash>/messages/index.jsonl
gmail/<account-hash>/messages/YYYY/MM/<timestamp>-<message-id>.eml
drive/<account-hash>/files/index.jsonl
drive/<account-hash>/files/<file-id>/<exported-file>
raw/<service>/...
```

`gog backup export` decrypts and verifies the manifest-backed shards before
writing files. Gmail messages become `.eml` files that open in Mail and other
mail clients. Drive content shards become normal files plus an index. Other
services are written as verified JSONL under `raw/`. The export is not
encrypted; do not place it inside the backup Git repository, and keep it out of
synced/shared folders unless that is intentional.

## Encryption

Backups use the Go `filippo.io/age` library with X25519 age identities. There
is no backup password. The private identity is stored locally:

```text
~/.gog/age.key
```

The matching public recipient starts with `age1...` and is safe to store in
`~/.gog/backup.json` and `manifest.json`. The private `AGE-SECRET-KEY-...`
value must stay local or in a password manager.

For each shard, `gog backup push`:

1. Exports deterministic JSONL rows.
2. Gzip-compresses the JSONL with a fixed gzip timestamp.
3. Encrypts the compressed bytes with age for every configured recipient.
4. Writes only encrypted `*.jsonl.gz.age` files to Git.
5. Writes `manifest.json` with cleartext metadata for status and verification.

`gog backup verify` decrypts each shard with the local age identity, gunzips it,
checks the plaintext SHA-256 hash from the manifest, and verifies row counts.
`gog backup cat` and `gog backup export` use the same verification path before
returning plaintext.

## Security Boundary

The encrypted shards protect Google content from GitHub and anyone else without
the local age identity. That includes email bodies, subjects, senders,
recipients, raw MIME payloads, labels, Drive filenames, contacts, event titles,
and similar service data.

The manifest is not secret. It leaks operational metadata by design:

- Export time.
- Public age recipients.
- Service names.
- Account hashes.
- Shard paths and month buckets.
- Row counts.
- Encrypted byte sizes.
- Plaintext shard hashes used by `verify`.
- Backup cadence and which shards changed in Git history.

The account hash is not anonymity. It is useful to avoid putting the literal
email address in paths, but someone who can guess the address can compute and
compare the same hash.

Current trust model:

- Confidentiality: good for a private GitHub backup repo as long as
  `~/.gog/age.key` stays private.
- Integrity against random corruption: `age` authentication, gzip decoding,
  plaintext SHA-256, and row-count verification catch damaged shards.
- Integrity against repository writers: limited. Anyone with push access can
  replace encrypted backup data with different data encrypted to the public
  recipient. Keep repo write access restricted and review unexpected commits.
- Key compromise: if `AGE-SECRET-KEY-...` leaks, historical shards in Git
  history may be readable. Rotate recipients, re-encrypt, and treat old Git
  history as exposed unless it is rewritten and all copies are removed.

Future hardening ideas:

- Store only ciphertext hashes in the public manifest and move plaintext hashes
  into encrypted shard metadata.
- Sign manifests or commits with a local signing key so `verify` can prove who
  created the backup, not just that the files are internally consistent.
- Add optional shard padding or disable gzip for deployments that care more
  about size side channels than repository size.

## Service Adapters

The Gmail adapter backs up:

- Gmail labels.
- Raw Gmail messages from `users.messages.get(format=raw)`.

Raw message payloads stay base64url encoded inside encrypted JSONL. This
preserves the RFC 2822 message content while keeping the shard format text
friendly.

`--include-spam-trash` defaults to true. Use `--query` and `--max` for bounded
test exports; omit them for a full mailbox scan.

The Gmail settings adapter backs up account configuration through read-only
settings endpoints. Some settings, such as delegates, can be forbidden for
consumer accounts; those errors are kept inside the encrypted settings shard.

The Calendar adapter backs up calendar list entries and all events from each
calendar. The Contacts adapter backs up contacts and other contacts. The Tasks
adapter backs up task lists and tasks. The Drive adapter backs up shared drives,
file metadata, and Google-native file exports by default. Content rows store
base64 bytes inside encrypted JSONL so Git only sees ciphertext; plaintext
export decodes them back into regular files. Non-Google binary Drive bytes are
opt-in because personal Drives can easily contain tens of gigabytes.

The Workspace adapter discovers Google Docs, Sheets, Slides, and Forms via
Drive. Docs/Sheets/Slides are already recoverable through the Drive content
exports; `--workspace-native` adds heavier native API JSON for machine-readable
recovery. The Apps Script adapter discovers script projects through Drive and
stores project metadata plus source content. Chat and Classroom adapters
enumerate data visible to the authenticated account; personal/permission-limited
accounts may produce encrypted error shards under `--best-effort`.

## Adding Services

Keep one backup engine and add service adapters. A service adapter should:

1. Resolve the authenticated account through normal `gog` auth.
2. Export stable rows without writing Google data.
3. Store sensitive identifiers, titles, names, and content inside encrypted
   shards only.
4. Add cleartext manifest counts and account hashes only.
5. Support bounded smoke flags when the service can be huge.

Good next adapters: Drive file content export, Docs/Sheets/Slides native
exports, Chat, Forms, Classroom, and Apps Script.
