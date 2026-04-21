package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := New(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, func() {
		s.db.Close()
		os.Remove(dbPath)
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
	}
}

func TestDomainHostnameCRUD(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	t.Run("AddDomain with hostname", func(t *testing.T) {
		d, err := s.AddDomain(ctx, "test.example.com", "mail.xxx.yyy")
		if err != nil {
			t.Fatalf("AddDomain: %v", err)
		}
		if d.Hostname != "mail.xxx.yyy" {
			t.Errorf("hostname = %q, want %q", d.Hostname, "mail.xxx.yyy")
		}
		if d.Domain != "test.example.com" {
			t.Errorf("domain = %q, want %q", d.Domain, "test.example.com")
		}
	})

	t.Run("AddDomain empty hostname", func(t *testing.T) {
		d, err := s.AddDomain(ctx, "empty.example.com", "")
		if err != nil {
			t.Fatalf("AddDomain: %v", err)
		}
		if d.Hostname != "" {
			t.Errorf("hostname = %q, want empty", d.Hostname)
		}
	})

	t.Run("AddDomainPending with hostname", func(t *testing.T) {
		d, err := s.AddDomainPending(ctx, "pending.example.com", "mail.host.zzz")
		if err != nil {
			t.Fatalf("AddDomainPending: %v", err)
		}
		if d.Hostname != "mail.host.zzz" {
			t.Errorf("hostname = %q, want %q", d.Hostname, "mail.host.zzz")
		}
		if d.Status != "pending" {
			t.Errorf("status = %q, want pending", d.Status)
		}
	})

	t.Run("GetDomainByID returns hostname", func(t *testing.T) {
		_, err := s.AddDomain(ctx, "getby.example.com", "mail.getby.com")
		if err != nil {
			t.Fatal(err)
		}
		domains, err := s.ListDomains(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, d := range domains {
			if d.Domain == "getby.example.com" {
				found = true
				if d.Hostname != "mail.getby.com" {
					t.Errorf("ListDomains hostname = %q, want %q", d.Hostname, "mail.getby.com")
				}
			}
		}
		if !found {
			t.Error("domain not found in ListDomains")
		}
	})

	t.Run("UpdateDomainHostname", func(t *testing.T) {
		_, err := s.AddDomain(ctx, "update.example.com", "old.hostname.com")
		if err != nil {
			t.Fatal(err)
		}
		domains, _ := s.ListDomains(ctx)
		var id int
		for _, d := range domains {
			if d.Domain == "update.example.com" {
				id = d.ID
			}
		}
		if id == 0 {
			t.Fatal("domain not found")
		}
		if err := s.UpdateDomainHostname(ctx, id, "new.hostname.com"); err != nil {
			t.Fatalf("UpdateDomainHostname: %v", err)
		}
		d, err := s.GetDomainByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Hostname != "new.hostname.com" {
			t.Errorf("hostname after update = %q, want %q", d.Hostname, "new.hostname.com")
		}
	})

	t.Run("GetActiveDomains includes hostname", func(t *testing.T) {
		_, err := s.AddDomain(ctx, "active.example.com", "mail.active.com")
		if err != nil {
			t.Fatal(err)
		}
		active, err := s.GetActiveDomains(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range active {
			if d.Domain == "active.example.com" && d.Hostname == "mail.active.com" {
				found = true
			}
		}
		if !found {
			t.Error("domain with hostname not found in GetActiveDomains")
		}
	})
}

func TestMigrationExistingDB(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	_, err := s.AddDomain(ctx, "migrated.example.com", "mail.migrated.com")
	if err != nil {
		t.Fatalf("AddDomain on fresh DB: %v", err)
	}
	d, err := s.GetDomainByName(ctx, "migrated.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if d.Hostname != "mail.migrated.com" {
		t.Errorf("hostname after fresh DB = %q, want %q", d.Hostname, "mail.migrated.com")
	}
}

func TestRetainedMailCRUD(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()
	ctx := context.Background()

	inserted, err := s.InsertRetainedMail(
		ctx,
		" User@Test.Example ",
		"sender@example.com",
		"Retained subject",
		"plain body",
		"<p>plain body</p>",
		"raw-message",
	)
	if err != nil {
		t.Fatalf("InsertRetainedMail: %v", err)
	}

	if inserted.RecipientAddress != "user@test.example" {
		t.Fatalf("recipient_address = %q, want %q", inserted.RecipientAddress, "user@test.example")
	}

	list, total, err := s.ListRetainedMails(ctx, 1, 20)
	if err != nil {
		t.Fatalf("ListRetainedMails: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].RecipientAddress != "user@test.example" {
		t.Fatalf("list recipient_address = %q, want %q", list[0].RecipientAddress, "user@test.example")
	}

	got, err := s.GetRetainedMail(ctx, inserted.ID)
	if err != nil {
		t.Fatalf("GetRetainedMail: %v", err)
	}
	if got.Subject != "Retained subject" {
		t.Fatalf("subject = %q, want %q", got.Subject, "Retained subject")
	}
	if got.BodyText != "plain body" {
		t.Fatalf("body_text = %q, want %q", got.BodyText, "plain body")
	}
	if got.RawMessage != "raw-message" {
		t.Fatalf("raw_message = %q, want %q", got.RawMessage, "raw-message")
	}

	if err := s.DeleteRetainedMail(ctx, inserted.ID); err != nil {
		t.Fatalf("DeleteRetainedMail: %v", err)
	}

	list, total, err = s.ListRetainedMails(ctx, 1, 20)
	if err != nil {
		t.Fatalf("ListRetainedMails after delete: %v", err)
	}
	if total != 0 {
		t.Fatalf("total after delete = %d, want 0", total)
	}
	if len(list) != 0 {
		t.Fatalf("len(list) after delete = %d, want 0", len(list))
	}
}
