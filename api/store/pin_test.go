package store

import (
	"context"
	"testing"
	"time"
)
func TestMailboxPinLifecycle(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	acct, err := s.CreateAccount(ctx, "testuser")
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	d, err := s.AddDomain(ctx, "pin.example.com", "mail.x.com")
	if err != nil {
		t.Fatalf("AddDomain: %v", err)
	}

	// Create mailbox (not pinned, 30min TTL)
	mb, err := s.CreateMailbox(ctx, acct.ID, "alice", d.ID, "alice@pin.example.com", 30)
	if err != nil {
		t.Fatalf("CreateMailbox: %v", err)
	}
	if mb.IsPinned {
		t.Errorf("new mailbox IsPinned = true, want false")
	}
	origExpires := mb.ExpiresAt

	// Pin it → is_pinned=true, expires_at unchanged
	pinned, err := s.SetMailboxPinned(ctx, mb.ID, acct.ID, true, 30)
	if err != nil {
		t.Fatalf("SetMailboxPinned(true): %v", err)
	}
	if !pinned.IsPinned {
		t.Errorf("after pin IsPinned = false, want true")
	}
	// expires_at 应保持不变（允许微秒级差异，因为 SQLite 存储精度）
	drift := pinned.ExpiresAt.Sub(origExpires)
	if drift < 0 {
		drift = -drift
	}
	if drift > time.Second {
		t.Errorf("ExpiresAt drifted too much on pin: got %v, want ~%v", pinned.ExpiresAt, origExpires)
	}

	// Verify it's still returned by GetMailbox
	got, err := s.GetMailbox(ctx, mb.ID, acct.ID)
	if err != nil {
		t.Fatalf("GetMailbox: %v", err)
	}
	if !got.IsPinned {
		t.Errorf("GetMailbox IsPinned = false, want true")
	}

	// Unpin → is_pinned=false, expires_at reset to ~now+30min
	unpinned, err := s.SetMailboxPinned(ctx, mb.ID, acct.ID, false, 30)
	if err != nil {
		t.Fatalf("SetMailboxPinned(false): %v", err)
	}
	if unpinned.IsPinned {
		t.Errorf("after unpin IsPinned = true, want false")
	}
	if unpinned.ExpiresAt.Before(time.Now().UTC().Add(29*time.Minute)) ||
		unpinned.ExpiresAt.After(time.Now().UTC().Add(31*time.Minute)) {
		t.Errorf("ExpiresAt after unpin not reset to ~now+30min: got %v", unpinned.ExpiresAt)
	}

	// Pin again → expiry cleanup shouldn't delete it
	if _, err := s.SetMailboxPinned(ctx, mb.ID, acct.ID, true, 30); err != nil {
		t.Fatalf("SetMailboxPinned(true) again: %v", err)
	}
	// Force expires_at into the past to simulate an expired pinned mailbox
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mailboxes SET expires_at = datetime('now', '-1 hour') WHERE id = ?`, mb.ID.String()); err != nil {
		t.Fatalf("force expire: %v", err)
	}
	deleted, err := s.DeleteExpiredMailboxes(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredMailboxes: %v", err)
	}
	if deleted != 0 {
		t.Errorf("DeleteExpiredMailboxes removed %d mailboxes, want 0 (pinned should survive)", deleted)
	}

	// Confirm it's still there
	if _, err := s.GetMailbox(ctx, mb.ID, acct.ID); err != nil {
		t.Fatalf("pinned mailbox was deleted by expiry cleanup: %v", err)
	}

	// Unpin → now it should be deletable (expired)
	if _, err := s.SetMailboxPinned(ctx, mb.ID, acct.ID, false, 30); err != nil {
		t.Fatalf("unpin before expiry test: %v", err)
	}
	// Force expires_at into the past again
	if _, err := s.db.ExecContext(ctx,
		`UPDATE mailboxes SET expires_at = datetime('now', '-1 hour') WHERE id = ?`, mb.ID.String()); err != nil {
		t.Fatalf("re-force expire: %v", err)
	}
	deleted, err = s.DeleteExpiredMailboxes(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredMailboxes after unpin: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 deleted mailbox, got %d", deleted)
	}
}
