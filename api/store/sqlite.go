package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tempmail/model"

	_ "github.com/glebarez/sqlite"
	"github.com/google/uuid"
)

type Store struct {
	db *sql.DB
}

var initSQL = `
CREATE TABLE IF NOT EXISTS accounts (
    id          TEXT PRIMARY KEY,
    username    TEXT NOT NULL UNIQUE,
    api_key     TEXT NOT NULL UNIQUE,
    is_admin    INTEGER NOT NULL DEFAULT 0,
    is_active   INTEGER NOT NULL DEFAULT 1,
    created_at  DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at  DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_accounts_api_key ON accounts (api_key);

CREATE TABLE IF NOT EXISTS domains (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    domain        TEXT NOT NULL UNIQUE,
    hostname      TEXT NOT NULL DEFAULT '',
    is_active     INTEGER NOT NULL DEFAULT 1,
    status        TEXT NOT NULL DEFAULT 'active',
    mx_checked_at DATETIME,
    created_at    DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_domains_active ON domains (is_active) WHERE is_active = 1;
CREATE INDEX IF NOT EXISTS idx_domains_status ON domains (status) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS mailboxes (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    address      TEXT NOT NULL,
    domain_id    INTEGER NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    full_address TEXT NOT NULL UNIQUE,
    created_at   DATETIME NOT NULL DEFAULT (datetime('now')),
    expires_at   DATETIME NOT NULL DEFAULT (datetime('now', '+30 minutes'))
);
CREATE INDEX IF NOT EXISTS idx_mailboxes_account_id ON mailboxes (account_id);
CREATE INDEX IF NOT EXISTS idx_mailboxes_expires_at ON mailboxes (expires_at);

CREATE TABLE IF NOT EXISTS emails (
    id           TEXT PRIMARY KEY,
    mailbox_id   TEXT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    sender       TEXT NOT NULL DEFAULT '',
    subject      TEXT NOT NULL DEFAULT '',
    body_text    TEXT NOT NULL DEFAULT '',
    body_html    TEXT NOT NULL DEFAULT '',
    raw_message  TEXT NOT NULL DEFAULT '',
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    received_at  DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_emails_mailbox_received ON emails (mailbox_id, received_at DESC);

CREATE TABLE IF NOT EXISTS retained_mails (
    id                TEXT PRIMARY KEY,
    recipient_address TEXT NOT NULL,
    sender            TEXT NOT NULL DEFAULT '',
    subject           TEXT NOT NULL DEFAULT '',
    body_text         TEXT NOT NULL DEFAULT '',
    body_html         TEXT NOT NULL DEFAULT '',
    raw_message       TEXT NOT NULL DEFAULT '',
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    received_at       DATETIME NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_retained_mails_received ON retained_mails (received_at DESC);
CREATE INDEX IF NOT EXISTS idx_retained_mails_recipient ON retained_mails (recipient_address);

CREATE TABLE IF NOT EXISTS app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
);

INSERT OR IGNORE INTO app_settings (key, value) VALUES ('registration_open', 'true');
INSERT OR IGNORE INTO app_settings (key, value) VALUES ('smtp_server_ip', '');
INSERT OR IGNORE INTO app_settings (key, value) VALUES ('smtp_hostname', '');
INSERT OR IGNORE INTO app_settings (key, value) VALUES ('mailbox_ttl_minutes', '30');
`

var migrateSQL = `
ALTER TABLE domains ADD COLUMN hostname TEXT NOT NULL DEFAULT '';
`

func New(ctx context.Context, dbPath string) (*Store, error) {
	if dbPath == "" {
		dbPath = "/data/tempmail.db"
	}

	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=-64000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("pragma %s: %w", p, err)
		}
	}

	if _, err := db.ExecContext(ctx, initSQL); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// Migrate: add hostname column to domains (safe to run repeatedly)
	db.ExecContext(ctx, migrateSQL)

	// Migrate: backfill hostname from old smtp_hostname setting
	var oldHostname string
	if err := db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = 'smtp_hostname'`).Scan(&oldHostname); err == nil && oldHostname != "" {
		db.ExecContext(ctx, `UPDATE domains SET hostname = ? WHERE hostname = ''`, oldHostname)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE is_admin = 1`).Scan(&count); err != nil {
		return nil, fmt.Errorf("check admin: %w", err)
	}
	if count == 0 {
		adminKey := "tm_admin_" + generateAPIKey()
		adminID := uuid.New().String()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO accounts (id, username, api_key, is_admin) VALUES (?, ?, ?, 1)`,
			adminID, "admin", adminKey,
		); err != nil {
			return nil, fmt.Errorf("seed admin: %w", err)
		}
	}

	db.SetMaxOpenConns(1)

	return &Store{db: db}, nil
}

func (s *Store) Close() {
	s.db.Close()
}

func (s *Store) GetAccountByAPIKey(ctx context.Context, apiKey string) (*model.Account, error) {
	var a model.Account
	var id, createdAt, updatedAt string
	var isAdmin, isActive int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, api_key, is_admin, is_active, created_at, updated_at
		 FROM accounts WHERE api_key = ? AND is_active = 1`, apiKey,
	).Scan(&id, &a.Username, &a.APIKey, &isAdmin, &isActive, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	a.ID = parseUUID(id)
	a.IsAdmin = isAdmin == 1
	a.IsActive = isActive == 1
	a.CreatedAt = parseTime(createdAt)
	a.UpdatedAt = parseTime(updatedAt)
	return &a, nil
}

func (s *Store) CreateAccount(ctx context.Context, username string) (*model.Account, error) {
	apiKey := generateAPIKey()
	id := uuid.New()
	now := time.Now().UTC()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, username, api_key) VALUES (?, ?, ?)`,
		id.String(), username, apiKey,
	)
	if err != nil {
		return nil, err
	}

	return &model.Account{
		ID: id, Username: username, APIKey: apiKey,
		IsAdmin: false, IsActive: true, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Store) DeleteAccount(ctx context.Context, accountID uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, accountID.String())
	return err
}

func (s *Store) ListAccounts(ctx context.Context, page, size int) ([]model.Account, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, api_key, is_admin, is_active, created_at, updated_at
		 FROM accounts ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		size, (page-1)*size,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var accounts []model.Account
	for rows.Next() {
		var a model.Account
		var id, createdAt, updatedAt string
		var isAdmin, isActive int
		if err := rows.Scan(&id, &a.Username, &a.APIKey, &isAdmin, &isActive, &createdAt, &updatedAt); err != nil {
			return nil, 0, err
		}
		a.ID = parseUUID(id)
		a.IsAdmin = isAdmin == 1
		a.IsActive = isActive == 1
		a.CreatedAt = parseTime(createdAt)
		a.UpdatedAt = parseTime(updatedAt)
		accounts = append(accounts, a)
	}
	return accounts, total, rows.Err()
}

func (s *Store) GetAdminAPIKey(ctx context.Context) (string, error) {
	var apiKey string
	err := s.db.QueryRowContext(ctx,
		`SELECT api_key FROM accounts WHERE is_admin = 1 ORDER BY created_at LIMIT 1`,
	).Scan(&apiKey)
	return apiKey, err
}

// ==================== Domain ====================

func (s *Store) AddDomain(ctx context.Context, domain, hostname string) (*model.Domain, error) {
	domain = strings.ToLower(domain)
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO domains (domain, hostname, is_active, status) VALUES (?, ?, 1, 'active')`, domain, hostname,
	)
	if err != nil {
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &model.Domain{
		ID: int(id), Domain: domain, Hostname: hostname, IsActive: true, Status: "active", CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *Store) AddDomainPending(ctx context.Context, domain, hostname string) (*model.Domain, error) {
	domain = strings.ToLower(domain)
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO domains (domain, hostname, is_active, status) VALUES (?, ?, 0, 'pending')`, domain, hostname,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate") {
			return s.GetDomainByName(context.Background(), domain)
		}
		return nil, err
	}
	id, _ := result.LastInsertId()
	return &model.Domain{
		ID: int(id), Domain: domain, Hostname: hostname, IsActive: false, Status: "pending", CreatedAt: time.Now().UTC(),
	}, nil
}

func (s *Store) ListDomains(ctx context.Context) ([]model.Domain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, domain, hostname, is_active, status, created_at, mx_checked_at FROM domains ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDomains(rows)
}

func (s *Store) GetActiveDomains(ctx context.Context) ([]model.Domain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, domain, hostname, is_active, status, created_at, mx_checked_at FROM domains WHERE is_active = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDomains(rows)
}

func (s *Store) GetRandomActiveDomain(ctx context.Context) (*model.Domain, error) {
	var d model.Domain
	var isActive int
	var createdAt, mxCheckedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, domain, hostname, is_active, status, created_at, mx_checked_at FROM domains
		 WHERE is_active = 1 ORDER BY RANDOM() LIMIT 1`,
	).Scan(&d.ID, &d.Domain, &d.Hostname, &isActive, &d.Status, &createdAt, &mxCheckedAt)
	if err != nil {
		return nil, err
	}
	d.IsActive = isActive == 1
	d.CreatedAt = parseNullTime(createdAt)
	d.MxCheckedAt = parseNullPtrTime(mxCheckedAt)
	return &d, nil
}

func (s *Store) GetDomainByName(ctx context.Context, domain string) (*model.Domain, error) {
	var d model.Domain
	var isActive int
	var createdAt, mxCheckedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, domain, hostname, is_active, status, created_at, mx_checked_at
		 FROM domains WHERE domain = ? AND is_active = 1`,
		strings.ToLower(domain),
	).Scan(&d.ID, &d.Domain, &d.Hostname, &isActive, &d.Status, &createdAt, &mxCheckedAt)
	if err != nil {
		return nil, err
	}
	d.IsActive = isActive == 1
	d.CreatedAt = parseNullTime(createdAt)
	d.MxCheckedAt = parseNullPtrTime(mxCheckedAt)
	return &d, nil
}

func (s *Store) GetDomainByID(ctx context.Context, domainID int) (*model.Domain, error) {
	var d model.Domain
	var isActive int
	var createdAt, mxCheckedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, domain, hostname, is_active, status, created_at, mx_checked_at FROM domains WHERE id = ?`,
		domainID,
	).Scan(&d.ID, &d.Domain, &d.Hostname, &isActive, &d.Status, &createdAt, &mxCheckedAt)
	if err != nil {
		return nil, err
	}
	d.IsActive = isActive == 1
	d.CreatedAt = parseNullTime(createdAt)
	d.MxCheckedAt = parseNullPtrTime(mxCheckedAt)
	return &d, nil
}

func (s *Store) ListPendingDomains(ctx context.Context) ([]model.Domain, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, domain, hostname, is_active, status, created_at, mx_checked_at
		 FROM domains WHERE status = 'pending' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDomains(rows)
}

func (s *Store) PromoteDomainToActive(ctx context.Context, domainID int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains SET is_active = 1, status = 'active', mx_checked_at = datetime('now') WHERE id = ?`, domainID)
	return err
}

func (s *Store) TouchDomainCheckTime(ctx context.Context, domainID int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains SET mx_checked_at = datetime('now') WHERE id = ?`, domainID)
	return err
}

func (s *Store) DisableDomainMX(ctx context.Context, domainID int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains SET is_active = 0, status = 'disabled', mx_checked_at = datetime('now') WHERE id = ?`, domainID)
	return err
}

func (s *Store) DeleteDomain(ctx context.Context, domainID int) error {
	// 先删除关联的 mailboxes（emails 通过 ON DELETE CASCADE 自动级联删除）
	if _, err := s.db.ExecContext(ctx, `DELETE FROM mailboxes WHERE domain_id = ?`, domainID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM domains WHERE id = ?`, domainID)
	return err
}

func (s *Store) ToggleDomain(ctx context.Context, domainID int, active bool) error {
	status := "disabled"
	if active {
		status = "active"
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains SET is_active = ?, status = ? WHERE id = ?`, boolToInt(active), status, domainID)
	return err
}

func (s *Store) UpdateDomainHostname(ctx context.Context, domainID int, hostname string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE domains SET hostname = ? WHERE id = ?`, hostname, domainID)
	return err
}

func (s *Store) GetStats(ctx context.Context) (*model.Stats, error) {
	var st model.Stats
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM mailboxes),
		  (SELECT COUNT(*) FROM mailboxes WHERE expires_at > datetime('now')),
		  (SELECT COUNT(*) FROM emails),
		  (SELECT COUNT(*) FROM domains WHERE is_active = 1),
		  (SELECT COUNT(*) FROM domains WHERE status = 'pending'),
		  (SELECT COUNT(*) FROM accounts WHERE is_active = 1)
	`).Scan(
		&st.TotalMailboxes, &st.ActiveMailboxes,
		&st.TotalEmails, &st.ActiveDomains,
		&st.PendingDomains, &st.TotalAccounts,
	)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// ==================== Mailbox ====================

func (s *Store) CreateMailbox(ctx context.Context, accountID uuid.UUID, address string, domainID int, fullAddress string, ttlMinutes int) (*model.Mailbox, error) {
	if ttlMinutes <= 0 {
		ttlMinutes = 30
	}
	id := uuid.New()
	expiresAt := time.Now().UTC().Add(time.Duration(ttlMinutes) * time.Minute)
	now := time.Now().UTC()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mailboxes (id, account_id, address, domain_id, full_address, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id.String(), accountID.String(), address, domainID, fullAddress, expiresAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}

	return &model.Mailbox{
		ID: id, AccountID: accountID, Address: address, DomainID: domainID,
		FullAddress: fullAddress, CreatedAt: now, ExpiresAt: expiresAt,
	}, nil
}

func (s *Store) ListMailboxes(ctx context.Context, accountID uuid.UUID, page, size int) ([]model.Mailbox, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mailboxes WHERE account_id = ?`, accountID.String()).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, account_id, address, domain_id, full_address, created_at, expires_at
		 FROM mailboxes WHERE account_id = ?
		 ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		accountID.String(), size, (page-1)*size,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var mailboxes []model.Mailbox
	for rows.Next() {
		var m model.Mailbox
		var id, acctID, createdAt, expiresAt string
		if err := rows.Scan(&id, &acctID, &m.Address, &m.DomainID, &m.FullAddress, &createdAt, &expiresAt); err != nil {
			return nil, 0, err
		}
		m.ID = parseUUID(id)
		m.AccountID = parseUUID(acctID)
		m.CreatedAt = parseTime(createdAt)
		m.ExpiresAt = parseTime(expiresAt)
		mailboxes = append(mailboxes, m)
	}
	return mailboxes, total, rows.Err()
}

func (s *Store) GetMailbox(ctx context.Context, mailboxID uuid.UUID, accountID uuid.UUID) (*model.Mailbox, error) {
	var m model.Mailbox
	var id, acctID, createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, account_id, address, domain_id, full_address, created_at, expires_at
		 FROM mailboxes WHERE id = ? AND account_id = ?`,
		mailboxID.String(), accountID.String(),
	).Scan(&id, &acctID, &m.Address, &m.DomainID, &m.FullAddress, &createdAt, &expiresAt)
	if err != nil {
		return nil, err
	}
	m.ID = parseUUID(id)
	m.AccountID = parseUUID(acctID)
	m.CreatedAt = parseTime(createdAt)
	m.ExpiresAt = parseTime(expiresAt)
	return &m, nil
}

func (s *Store) DeleteMailbox(ctx context.Context, mailboxID uuid.UUID, accountID uuid.UUID) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM mailboxes WHERE id = ? AND account_id = ?`, mailboxID.String(), accountID.String())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) GetMailboxByFullAddress(ctx context.Context, fullAddress string) (*model.Mailbox, error) {
	var m model.Mailbox
	var id, acctID, createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, account_id, address, domain_id, full_address, created_at, expires_at
		 FROM mailboxes WHERE full_address = ?`,
		strings.ToLower(fullAddress),
	).Scan(&id, &acctID, &m.Address, &m.DomainID, &m.FullAddress, &createdAt, &expiresAt)
	if err != nil {
		return nil, err
	}
	m.ID = parseUUID(id)
	m.AccountID = parseUUID(acctID)
	m.CreatedAt = parseTime(createdAt)
	m.ExpiresAt = parseTime(expiresAt)
	return &m, nil
}

func (s *Store) DeleteExpiredMailboxes(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM mailboxes WHERE expires_at < datetime('now')`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func CheckDomainMX(domain, serverIP string) (matched bool, mxHosts []string, status string) {
	mxRecords, err := net.LookupMX(domain)
	if err != nil {
		return false, nil, fmt.Sprintf("DNS查询失败: %v", err)
	}
	if len(mxRecords) == 0 {
		return false, nil, "未找到MX记录，请先配置MX记录"
	}
	for _, mx := range mxRecords {
		host := strings.TrimSuffix(mx.Host, ".")
		mxHosts = append(mxHosts, host)
		addrs, err := net.LookupHost(host)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if addr == serverIP {
				return true, mxHosts, fmt.Sprintf("✓ MX记录匹配：%s → %s", host, addr)
			}
		}
	}
	return false, mxHosts, fmt.Sprintf("MX记录(%s)未指向本服务器(%s)", strings.Join(mxHosts, ","), serverIP)
}

// ==================== Email ====================

func (s *Store) InsertEmail(ctx context.Context, mailboxID uuid.UUID, sender, subject, bodyText, bodyHTML, raw string) (*model.Email, error) {
	id := uuid.New()
	now := time.Now().UTC()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO emails (id, mailbox_id, sender, subject, body_text, body_html, raw_message, size_bytes, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), mailboxID.String(), sender, subject, bodyText, bodyHTML, raw, len(raw), now.Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}

	return &model.Email{
		ID: id, MailboxID: mailboxID, Sender: sender, Subject: subject,
		BodyText: bodyText, BodyHTML: bodyHTML, RawMessage: raw,
		SizeBytes: len(raw), ReceivedAt: now,
	}, nil
}

func (s *Store) ListEmails(ctx context.Context, mailboxID uuid.UUID, page, size int) ([]model.EmailSummary, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM emails WHERE mailbox_id = ?`, mailboxID.String()).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, sender, subject, size_bytes, received_at
		 FROM emails WHERE mailbox_id = ?
		 ORDER BY received_at DESC LIMIT ? OFFSET ?`,
		mailboxID.String(), size, (page-1)*size,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var emails []model.EmailSummary
	for rows.Next() {
		var e model.EmailSummary
		var id, receivedAt string
		if err := rows.Scan(&id, &e.Sender, &e.Subject, &e.SizeBytes, &receivedAt); err != nil {
			return nil, 0, err
		}
		e.ID = parseUUID(id)
		e.ReceivedAt = parseTime(receivedAt)
		emails = append(emails, e)
	}
	return emails, total, rows.Err()
}

func (s *Store) GetEmail(ctx context.Context, emailID uuid.UUID, mailboxID uuid.UUID) (*model.Email, error) {
	var e model.Email
	var id, mboxID, receivedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, mailbox_id, sender, subject, body_text, body_html, raw_message, size_bytes, received_at
		 FROM emails WHERE id = ? AND mailbox_id = ?`,
		emailID.String(), mailboxID.String(),
	).Scan(&id, &mboxID, &e.Sender, &e.Subject, &e.BodyText, &e.BodyHTML, &e.RawMessage, &e.SizeBytes, &receivedAt)
	if err != nil {
		return nil, err
	}
	e.ID = parseUUID(id)
	e.MailboxID = parseUUID(mboxID)
	e.ReceivedAt = parseTime(receivedAt)
	return &e, nil
}

func (s *Store) DeleteEmail(ctx context.Context, emailID uuid.UUID, mailboxID uuid.UUID) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM emails WHERE id = ? AND mailbox_id = ?`, emailID.String(), mailboxID.String())
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) InsertRetainedMail(ctx context.Context, recipientAddress, sender, subject, bodyText, bodyHTML, raw string) (*model.RetainedMail, error) {
	id := uuid.New()
	now := time.Now().UTC()
	recipientAddress = strings.ToLower(strings.TrimSpace(recipientAddress))

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO retained_mails (id, recipient_address, sender, subject, body_text, body_html, raw_message, size_bytes, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), recipientAddress, sender, subject, bodyText, bodyHTML, raw, len(raw), now.Format(time.RFC3339),
	)
	if err != nil {
		return nil, err
	}

	return &model.RetainedMail{
		ID:               id,
		RecipientAddress: recipientAddress,
		Sender:           sender,
		Subject:          subject,
		BodyText:         bodyText,
		BodyHTML:         bodyHTML,
		RawMessage:       raw,
		SizeBytes:        len(raw),
		ReceivedAt:       now,
	}, nil
}

func (s *Store) ListRetainedMails(ctx context.Context, page, size int) ([]model.RetainedMailSummary, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM retained_mails`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, recipient_address, sender, subject, size_bytes, received_at
		 FROM retained_mails ORDER BY received_at DESC LIMIT ? OFFSET ?`,
		size, (page-1)*size,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var retained []model.RetainedMailSummary
	for rows.Next() {
		var mail model.RetainedMailSummary
		var id, receivedAt string
		if err := rows.Scan(&id, &mail.RecipientAddress, &mail.Sender, &mail.Subject, &mail.SizeBytes, &receivedAt); err != nil {
			return nil, 0, err
		}
		mail.ID = parseUUID(id)
		mail.ReceivedAt = parseTime(receivedAt)
		retained = append(retained, mail)
	}

	return retained, total, rows.Err()
}

func (s *Store) GetRetainedMail(ctx context.Context, retainedMailID uuid.UUID) (*model.RetainedMail, error) {
	var mail model.RetainedMail
	var id, receivedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, recipient_address, sender, subject, body_text, body_html, raw_message, size_bytes, received_at
		 FROM retained_mails WHERE id = ?`,
		retainedMailID.String(),
	).Scan(&id, &mail.RecipientAddress, &mail.Sender, &mail.Subject, &mail.BodyText, &mail.BodyHTML, &mail.RawMessage, &mail.SizeBytes, &receivedAt)
	if err != nil {
		return nil, err
	}
	mail.ID = parseUUID(id)
	mail.ReceivedAt = parseTime(receivedAt)
	return &mail, nil
}

// ==================== Helpers ====================

func generateAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func GenerateRandomAddress() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	length := 10
	result := make([]byte, length)
	for i := range result {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		result[i] = chars[n.Int64()]
	}
	return string(result)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	if t.IsZero() {
		t, _ = time.Parse("2006-01-02 15:04:05", s)
	}
	return t
}

func parseNullTime(ns sql.NullString) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	return parseTime(ns.String)
}

func parseNullPtrTime(ns sql.NullString) *time.Time {
	if !ns.Valid {
		return nil
	}
	t := parseTime(ns.String)
	return &t
}

func scanDomains(rows *sql.Rows) ([]model.Domain, error) {
	var domains []model.Domain
	for rows.Next() {
		var d model.Domain
		var isActive int
		var createdAt, mxCheckedAt sql.NullString
		if err := rows.Scan(&d.ID, &d.Domain, &d.Hostname, &isActive, &d.Status, &createdAt, &mxCheckedAt); err != nil {
			return nil, err
		}
		d.IsActive = isActive == 1
		d.CreatedAt = parseNullTime(createdAt)
		d.MxCheckedAt = parseNullPtrTime(mxCheckedAt)
		domains = append(domains, d)
	}
	return domains, rows.Err()
}
