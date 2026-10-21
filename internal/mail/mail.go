// Package mail envoie les e-mails du serveur (facultatif, LATTIS_SMTP_*) :
// la file lattis.mail_outbox est remplie par la base (invitations,
// demandes de lien) et par lattisd, puis servie ici, sous un plafond
// d'envoi par heure (tout le serveur et par destinataire). Le contenu d'un
// message est effacé de la file une fois envoyé.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/internal/config"
)

// Message : un e-mail en texte brut.
type Message struct {
	To, Subject, Body string
}

// Sender envoie un message (SMTP en production, faux en test).
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// SMTP envoie par un serveur SMTP.
type SMTP struct {
	Cfg      config.Mail
	password string
}

// NewSMTP lit le mot de passe (fichier de secret) et prépare l'envoi.
func NewSMTP(cfg config.Mail) (*SMTP, error) {
	s := &SMTP{Cfg: cfg}
	if cfg.PasswordFile != "" {
		b, err := os.ReadFile(cfg.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("LATTIS_SMTP_PASSWORD_FILE : %w", err)
		}
		s.password = strings.TrimSpace(string(b))
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("LATTIS_SMTP_FROM : %w", err)
	}
	return s, nil
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, _ := mail.ParseAddress(s.Cfg.From)
	addr := net.JoinHostPort(s.Cfg.Host, strconv.Itoa(s.Cfg.Port))
	d := net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: s.Cfg.Host, MinVersion: tls.VersionTLS12}
	if s.Cfg.TLS == "tls" {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	}
	c, err := smtp.NewClient(conn, s.Cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.Cfg.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("le serveur SMTP ne propose pas STARTTLS (LATTIS_SMTP_TLS=none pour un relais local)")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if s.Cfg.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Cfg.User, s.password, s.Cfg.Host)); err != nil {
			return fmt.Errorf("authentification SMTP : %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(m.To); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(compose(s.Cfg.From, from.Address, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// compose : en-têtes et corps (UTF-8, quoted-printable).
func compose(fromHeader, fromAddr string, m Message) []byte {
	var b bytes.Buffer
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := fromAddr[strings.LastIndex(fromAddr, "@")+1:]
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\n",
		fromHeader, m.To, mime.QEncoding.Encode("utf-8", m.Subject), time.Now().Format(time.RFC1123Z), hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\nAuto-Submitted: auto-generated\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n")))
	_ = qp.Close()
	return b.Bytes()
}

// --- File d'envoi -----------------------------------------------------------------------

// Worker sert la file d'envoi.
type Worker struct {
	Pool   *pgxpool.Pool
	Sender Sender
	Cfg    config.Mail
	// URL : adresse publique d'un chemin (liens des messages).
	URL func(path string) string
	Now func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run sert la file jusqu'à la fin de ctx : réveil par NOTIFY lattis_mail,
// et relevé toutes les 30 s (messages retenus par le plafond).
func (w *Worker) Run(ctx context.Context) {
	wake := make(chan struct{}, 1)
	go w.listen(ctx, wake)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		for {
			n, err := w.Process(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Warn("envoi d'e-mails", "err", err)
			}
			if n == 0 || err != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

func (w *Worker) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		conn, err := w.Pool.Acquire(ctx)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN lattis_mail"); err == nil {
			for {
				if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
					break
				}
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
		conn.Conn().Close(context.Background())
		conn.Release()
	}
}

type queued struct {
	ID        int64
	Kind      string
	Recipient string
	Lang      string
	Data      map[string]any
	CreatedAt time.Time
}

// Process envoie les messages en attente qui tiennent sous le plafond ;
// renvoie le nombre envoyés ou abandonnés.
func (w *Worker) Process(ctx context.Context) (int, error) {
	done := 0
	for i := 0; i < 20; i++ {
		ok, err := w.one(ctx)
		if err != nil || !ok {
			return done, err
		}
		done++
	}
	return done, nil
}

func (w *Worker) one(ctx context.Context) (bool, error) {
	var q queued
	var sent, retry bool
	err := pgx.BeginFunc(ctx, w.Pool, func(tx pgx.Tx) error {
		// Messages trop vieux : abandonnés (liens devenus inutiles).
		if _, err := tx.Exec(ctx, `UPDATE lattis.mail_outbox SET status = 'expired', data = '{}'
			WHERE status = 'pending' AND created_at < now() - interval '1 day'`); err != nil {
			return err
		}
		var hour int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM lattis.mail_outbox WHERE status = 'sent' AND sent_at > now() - interval '1 hour'`).Scan(&hour); err != nil {
			return err
		}
		if w.Cfg.PerHour > 0 && hour >= w.Cfg.PerHour {
			return pgx.ErrNoRows // plafond du serveur : on attend
		}
		// Le plus ancien message dont le destinataire est sous son plafond.
		err := tx.QueryRow(ctx, `SELECT o.id, o.kind, o.recipient, o.lang, o.data, o.created_at FROM lattis.mail_outbox o
			WHERE o.status = 'pending' AND (SELECT count(*) FROM lattis.mail_outbox s WHERE s.recipient = o.recipient
			      AND s.status = 'sent' AND s.sent_at > now() - interval '1 hour') < $1
			ORDER BY o.id LIMIT 1 FOR UPDATE SKIP LOCKED`, max(w.Cfg.PerAddress, 1)).
			Scan(&q.ID, &q.Kind, &q.Recipient, &q.Lang, &q.Data, &q.CreatedAt)
		if err != nil {
			return err
		}
		msg, err := w.render(q)
		if err != nil {
			_, _ = tx.Exec(ctx, `UPDATE lattis.mail_outbox SET status = 'failed', last_error = $2, data = '{}' WHERE id = $1`, q.ID, err.Error())
			return nil
		}
		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		serr := w.Sender.Send(sctx, msg)
		cancel()
		if serr != nil {
			_, err := tx.Exec(ctx, `UPDATE lattis.mail_outbox SET attempts = attempts + 1, last_error = $2,
				status = CASE WHEN attempts + 1 >= 5 THEN 'failed' ELSE status END,
				data = CASE WHEN attempts + 1 >= 5 THEN '{}' ELSE data END WHERE id = $1`, q.ID, serr.Error())
			slog.Warn("envoi d'e-mail", "kind", q.Kind, "err", serr)
			retry = true // tentative notée (transaction validée), nouvel essai au prochain relevé
			return err
		}
		sent = true
		_, err = tx.Exec(ctx, `UPDATE lattis.mail_outbox SET status = 'sent', sent_at = now(), attempts = attempts + 1,
			last_error = NULL, data = '{}' WHERE id = $1`, q.ID)
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	case retry:
		return false, nil
	}
	if sent {
		slog.Info("e-mail envoyé", "kind", q.Kind)
	}
	return true, nil
}

// render : texte du message selon son type et sa langue.
func (w *Worker) render(q queued) (Message, error) {
	str := func(k string) string { v, _ := q.Data[k].(string); return v }
	server := str("server")
	if server == "" {
		server = "Serveur Lattis"
	}
	m := Message{To: q.Recipient}
	switch q.Kind {
	case "password_link":
		link := w.URL("/setup?token=" + str("token"))
		switch str("reason") {
		case "forgot":
			m.Subject = server + " : choisir un nouveau mot de passe"
			m.Body = "Bonjour,\n\nUne demande de nouveau mot de passe a été faite pour votre compte sur " + server + ".\n" +
				"Pour en choisir un, ouvrez ce lien (valable 7 jours, une seule fois) :\n\n" + link + "\n\n" +
				"Si vous n'avez rien demandé, ignorez ce message : votre mot de passe actuel reste valable."
		case "reset":
			m.Subject = server + " : votre mot de passe a été réinitialisé"
			m.Body = "Bonjour,\n\nL'administrateur de " + server + " a réinitialisé votre mot de passe.\n" +
				"Pour en choisir un nouveau, ouvrez ce lien (valable 7 jours, une seule fois) :\n\n" + link
		default:
			m.Subject = server + " : votre compte Lattis"
			m.Body = "Bonjour,\n\nUn compte a été créé pour vous sur " + server + ".\n" +
				"Pour choisir votre mot de passe, ouvrez ce lien (valable 7 jours, une seule fois) :\n\n" + link + "\n\n" +
				"Ensuite, ajoutez ce serveur dans l'application Lattis : " + w.URL("/")
		}
	case "confirm_email":
		m.Subject = server + " : confirmez votre adresse"
		m.Body = "Bonjour,\n\nPour confirmer que cette adresse est bien la vôtre sur " + server + ", ouvrez ce lien (valable 7 jours) :\n\n" +
			w.URL("/confirm?token="+str("token")) + "\n\n" +
			"Les invitations adressées à cette adresse vous seront alors ouvertes. Si vous n'êtes pas à l'origine de cette demande, ignorez ce message."
	case "invitation":
		if team := str("team"); team != "" {
			by := str("by")
			if by == "" {
				by = "Un membre"
			}
			m.Subject = server + " : invitation dans l'équipe « " + team + " »"
			m.Body = "Bonjour,\n\n" + by + " vous invite dans l'équipe « " + team + " » sur " + server + ".\n\n" +
				"Pour vous connecter : " + w.URL("/") + "\n\n" +
				"Connectez-vous avec cette adresse e-mail : vous y retrouverez les droits de l'équipe."
			break
		}
		m.Subject = server + " : invitation au classeur « " + str("title") + " »"
		by := str("by")
		if by == "" {
			by = "Un membre"
		}
		m.Body = "Bonjour,\n\n" + by + " vous invite au classeur « " + str("title") + " » sur " + server + ".\n\n" +
			"Pour l'ouvrir : " + w.URL("/w/"+str("workbook_id")) + "\n\n" +
			"Connectez-vous avec cette adresse e-mail pour y avoir accès."
	case "pipeline_failed":
		m.Subject = server + " : échec du pipeline « " + str("name") + " »"
		m.Body = "Le pipeline « " + str("name") + " » a échoué :\n\n" + str("error") + "\n\nDétail : " + w.URL("/admin/pipelines")
	default:
		return m, fmt.Errorf("type d'e-mail inconnu : %s", q.Kind)
	}
	m.Body += "\n\n-- \n" + server + " · " + w.URL("/") + "\n"
	return m, nil
}

// Test envoie tout de suite un message d'essai (bouton de l'administration).
func Test(ctx context.Context, s Sender, to, server, url string) error {
	return s.Send(ctx, Message{To: to, Subject: server + " : e-mail d'essai",
		Body: "Bonjour,\n\nCet e-mail d'essai confirme que " + server + " sait envoyer des e-mails.\n\n-- \n" + server + " · " + url + "\n"})
}

// Enqueue met un message en file depuis lattisd (connexion de service).
func Enqueue(ctx context.Context, pool *pgxpool.Pool, kind, to string, data map[string]any) error {
	b, _ := json.Marshal(data)
	_, err := pool.Exec(ctx, `SELECT lattis.mail_enqueue($1, $2, $3::jsonb)`, kind, to, string(b))
	return err
}
