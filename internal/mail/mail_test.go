package mail_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/config"
	"github.com/Be-Lattis/lattis-server/internal/mail"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (f *fakeSender) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

// File d'envoi : invitation, mot de passe oublié, confirmation d'adresse,
// plafond par destinataire, contenu effacé après envoi.
func TestOutbox(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	service := func(q string, dst any, args ...any) error {
		return db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
			if dst == nil {
				_, err := tx.Exec(ctx, q, args...)
				return err
			}
			return tx.QueryRow(ctx, q, args...).Scan(dst)
		})
	}
	// Sans envoi configuré : rien en file.
	must(service(`SELECT lattis.mail_enqueue('invitation', 'a@acme.fr', '{}')`, nil))
	var n int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM lattis.mail_outbox`).Scan(&n))
	if n != 0 {
		t.Fatal("message mis en file sans envoi configuré")
	}
	_, err := db.Super.Exec(ctx, `UPDATE lattis.server_config SET value = 'true' WHERE key = 'mail_enabled'`)
	must(err)

	var google string
	must(service(`SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`, &google))
	must(service(`SELECT lattis.set_config('access_mode', '"open"')`, nil))
	owner, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: "o", Email: testpg.Email("proprio"), EmailVerified: true})
	must(err)
	var wb string
	must(db.AsUser(t, owner.PGRole, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.create_workbook('Budget', 'collaborative')::text`).Scan(&wb); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.finish_publish($1::uuid)`, wb).Scan(new(int)); err != nil {
			return err
		}
		// Deux invitations au même destinataire : la seconde attend (plafond 1 par heure).
		if _, err := tx.Exec(ctx, `SELECT lattis.invite('invite@acme.fr', $1::uuid, ARRAY['reader'])`, wb); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT lattis.invite('invite@acme.fr', $1::uuid, ARRAY['editor'])`, wb)
		return err
	}))

	// Compte à mot de passe : « mot de passe oublié ».
	var token string
	must(service(`SELECT lattis.create_password_account('marc@acme.fr', 'Marc')`, &token))
	must(service(`SELECT lattis.request_password_reset('MARC@acme.fr')`, nil))
	must(service(`SELECT lattis.request_password_reset('inconnu@acme.fr')`, nil))

	f := &fakeSender{}
	w := &mail.Worker{Pool: db.Store.DB, Sender: f, Cfg: config.Mail{PerHour: 100, PerAddress: 1},
		URL: func(p string) string { return "https://lattis.acme.fr" + p }}
	_, err = w.Process(ctx)
	must(err)
	if len(f.sent) != 2 {
		t.Fatalf("messages envoyés : %d (%v)", len(f.sent), f.sent)
	}
	var inv, reset mail.Message
	for _, m := range f.sent {
		switch {
		case strings.Contains(m.Subject, "invitation"):
			inv = m
		case strings.Contains(m.Subject, "mot de passe"):
			reset = m
		}
	}
	if inv.To != "invite@acme.fr" || !strings.Contains(inv.Body, "https://lattis.acme.fr/w/"+wb) || !strings.Contains(inv.Subject, "Budget") {
		t.Fatalf("invitation : %+v", inv)
	}
	m := regexp.MustCompile(`/setup\?token=([0-9a-f]+)`).FindStringSubmatch(reset.Body)
	if reset.To != "marc@acme.fr" || m == nil {
		t.Fatalf("mot de passe oublié : %+v", reset)
	}
	// Le lien reçu permet de choisir un mot de passe.
	if _, err := db.Store.SetPasswordWithToken(ctx, m[1], "$argon2id$fake", ""); err != nil {
		t.Fatalf("lien reçu : %v", err)
	}
	var pending, withData int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'pending'), count(*) FILTER (WHERE status = 'sent' AND data <> '{}')
		FROM lattis.mail_outbox`).Scan(&pending, &withData))
	if pending != 1 || withData != 0 {
		t.Fatalf("file après envoi : %d en attente, %d envoyés avec contenu", pending, withData)
	}

	// Confirmation d'une adresse non vérifiée.
	var uid string
	must(db.Super.QueryRow(ctx, `UPDATE lattis.users SET email_verified = false WHERE email = 'marc@acme.fr' RETURNING id::text`).Scan(&uid))
	var ok bool
	must(service(`SELECT lattis.request_email_confirmation($1::uuid)`, &ok, uid))
	w.Cfg.PerAddress = 5
	_, err = w.Process(ctx)
	must(err)
	var confirm mail.Message
	for _, msg := range f.sent {
		if strings.Contains(msg.Subject, "confirmez") {
			confirm = msg
		}
	}
	cm := regexp.MustCompile(`/confirm\?token=([0-9a-f]+)`).FindStringSubmatch(confirm.Body)
	if !ok || cm == nil {
		t.Fatalf("confirmation : %v %+v", ok, confirm)
	}
	var email *string
	must(service(`SELECT lattis.confirm_email($1)`, &email, cm[1]))
	var verified bool
	must(db.Super.QueryRow(ctx, `SELECT email_verified FROM lattis.users WHERE id = $1`, uid).Scan(&verified))
	if email == nil || *email != "marc@acme.fr" || !verified {
		t.Fatalf("adresse confirmée : %v %v", email, verified)
	}
	must(service(`SELECT lattis.confirm_email($1)`, &email, cm[1]))
	if email != nil {
		t.Fatal("lien de confirmation réutilisable")
	}
}
