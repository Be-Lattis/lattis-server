package forms_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/forms"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

// Parcours d'un répondant : page, envoi trop rapide, envoi, une réponse par
// personne, accès refusé, aperçu, images.
func TestFormPage(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	var google string
	must(db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT lattis.set_config('access_mode', '"open"')`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT lattis.add_provider('google', 'Google', NULL, 'cid', NULL, NULL)::text`).Scan(&google)
	}))
	enroll := func(n string) store.Enrollment {
		u, err := db.Store.Enroll(ctx, store.Identity{ProviderID: google, Subject: n, Email: testpg.Email(n), EmailVerified: true})
		must(err)
		return u
	}
	owner, reader, outsider := enroll("auteur"), enroll("repondant"), enroll("dehors")
	as := func(u store.Enrollment, q string, dst any, args ...any) error {
		return db.AsUser(t, u.PGRole, func(tx pgx.Tx) error {
			if dst == nil {
				_, err := tx.Exec(ctx, q, args...)
				return err
			}
			return tx.QueryRow(ctx, q, args...).Scan(dst)
		})
	}
	var wb, tid, schema string
	must(as(owner, `SELECT lattis.create_workbook('Nids', 'collaborative')::text`, &wb))
	must(as(owner, `SELECT lattis.create_table($1::uuid, 'Relevés', '[{"name":"Commune","type":"text"},{"name":"Nombre","type":"integer"},{"name":"Reçu","type":"datetime"},{"name":"N","type":"integer"},{"name":"Par","type":"text"}]')::text`, &tid, wb))
	must(as(owner, `SELECT lattis.my_capabilities($1::uuid) ->> 'schema'`, &schema, wb))
	must(as(owner, `SELECT lattis.finish_publish($1::uuid)`, new(int), wb))
	must(as(owner, `SELECT lattis.invite($1, $2::uuid, ARRAY['reader'])`, nil, reader.Email, wb))
	col := func(name string) string {
		var id string
		must(db.Super.QueryRow(ctx, `SELECT column_id::text FROM `+pgx.Identifier{schema, "lattis_meta_columns"}.Sanitize()+` WHERE name = $1`, name).Scan(&id))
		return id
	}
	def := `{"appearance": {"title": "Relevé des nids", "thanks": "Merci !", "another": true},
		"fields": [{"column": "` + col("Commune") + `", "label": "Commune", "list": {"values": ["Brest", "Quimper"]}, "param": "commune", "locked": true},
		           {"column": "` + col("Nombre") + `", "label": "Nombre", "widget": "number", "required": true, "min": 0}],
		"server_values": [{"column": "` + col("Reçu") + `", "value": {"token": "received_at"}},
		                  {"column": "` + col("N") + `", "value": {"token": "next_number"}},
		                  {"column": "` + col("Par") + `", "value": {"token": "respondent"}}]}`
	var created map[string]string
	must(as(owner, `SELECT lattis.create_form($1::uuid, $2::uuid, $3::jsonb)`, &created, wb, tid, `{"max_per_respondent": 1, "definition": `+def+`}`))
	must(as(owner, `SELECT lattis.set_form_state($1::uuid, 'open')`, nil, created["id"]))
	code := created["code"]

	pem, _ := tokens.GenerateKeyPEM()
	keys, _ := tokens.ParseKeys(pem)
	// Serveur hors UTC : l'heure de réception garde l'heure locale, marquée UTC.
	clock := time.Date(2026, 10, 23, 14, 30, 0, 0, time.FixedZone("Paris", 2*3600))
	current := map[string]store.Enrollment{}
	who := ""
	h := &forms.Handler{Store: db.Store, Users: userdb.SessionAuth{Pool: db.Super}, Keys: keys, Now: func() time.Time { return clock },
		Session: func(*http.Request) (store.User, error) {
			if e, ok := current[who]; ok {
				return db.Store.User(ctx, e.UserID)
			}
			return store.User{}, errors.New("pas de session")
		},
		Bearer: func(*http.Request) (store.User, error) { return db.Store.User(ctx, owner.UserID) }}
	current["reader"], current["outsider"], current["owner"] = reader, outsider, owner
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path string, form url.Values) (int, string, http.Header) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequest(method, srv.URL+path, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(req)
		must(err)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b), resp.Header
	}
	tokenOf := func(page string) string {
		m := regexp.MustCompile(`name="t" value="([^"]+)"`).FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("jeton absent : %.300s", page)
		}
		return m[1]
	}

	// Anonyme : connexion demandée ; hors du classeur : introuvable.
	if c, _, hd := do("GET", "/f/"+code, nil); c != http.StatusFound || !strings.HasPrefix(hd.Get("Location"), "/admin/login?next=") {
		t.Fatalf("anonyme : %d %s", c, hd.Get("Location"))
	}
	who = "outsider"
	if c, b, _ := do("GET", "/f/"+code, nil); c != http.StatusNotFound || !strings.Contains(b, "introuvable") && !strings.Contains(b, "n'existe pas") {
		t.Fatalf("hors du classeur : %d", c)
	}
	who = "reader"
	c, page, _ := do("GET", "/f/"+code+"?commune=Brest", nil)
	if c != http.StatusOK || !strings.Contains(page, "Relevé des nids") || !strings.Contains(page, `value="Brest"`) {
		t.Fatalf("page : %d %.400s", c, page)
	}
	tok := tokenOf(page)
	// Valeur verrouillée par le lien : celle du corps est ignorée.
	form := url.Values{"t": {tok}, "c_" + col("Commune"): {"Quimper"}, "c_" + col("Nombre"): {"3"}}
	if c, b, _ := do("POST", "/f/"+code+"?commune=Brest", form); c != http.StatusBadRequest || !strings.Contains(b, "trop rapide") {
		t.Fatalf("envoi immédiat : %d", c)
	}
	clock = clock.Add(5 * time.Second)
	if c, b, _ := do("POST", "/f/"+code+"?commune=Brest", url.Values{"t": {tok}, "c_" + col("Nombre"): {"-2"}}); c != http.StatusBadRequest || !strings.Contains(b, "Au moins 0") {
		t.Fatalf("valeur hors bornes : %d %.300s", c, b)
	}
	c, _, hd := do("POST", "/f/"+code+"?commune=Brest", form)
	if c != http.StatusSeeOther || !strings.HasPrefix(hd.Get("Location"), "/f/"+code+"/merci") {
		t.Fatalf("envoi : %d %s", c, hd.Get("Location"))
	}
	if c, b, _ := do("GET", hd.Get("Location"), nil); c != http.StatusOK || !strings.Contains(b, "Merci !") || !strings.Contains(b, "/f/"+code+"?commune=Brest") {
		t.Fatalf("remerciement : %d %.300s", c, b)
	}
	var commune, author, by string
	var n int
	var recu time.Time
	must(db.Super.QueryRow(ctx, `SELECT "Commune", "Nombre", "Reçu", "N", "Par", lattis_author FROM `+pgx.Identifier{schema, "Relevés"}.Sanitize()).
		Scan(&commune, new(int), &recu, &n, &by, &author))
	if commune != "Brest" || n != 1 || by != reader.Email || !strings.HasPrefix(author, "form+") {
		t.Fatalf("ligne écrite : %s %d %s %s", commune, n, by, author)
	}
	if u := recu.UTC(); u.Hour() != 14 || u.Minute() != 30 {
		t.Fatalf("heure de réception : %v", recu)
	}
	// Une réponse par personne.
	_, page, _ = do("GET", "/f/"+code, nil)
	if !strings.Contains(page, "déjà répondu") {
		t.Fatalf("seconde visite : %.300s", page)
	}

	// Aperçu : ouvert à son concepteur, rien d'enregistré.
	var preview string
	must(as(owner, `SELECT lattis.form_preview_url($1::uuid)`, &preview, created["id"]))
	who = "outsider"
	c, page, _ = do("GET", preview, nil)
	if c != http.StatusOK || !strings.Contains(page, "Aperçu") {
		t.Fatalf("aperçu : %d", c)
	}
	clock = clock.Add(5 * time.Second)
	if c, b, _ := do("POST", preview, url.Values{"t": {tokenOf(page)}, "c_" + col("Commune"): {"Quimper"}, "c_" + col("Nombre"): {"1"}}); c != http.StatusOK || !strings.Contains(b, "pas été enregistrée") {
		t.Fatalf("envoi en aperçu : %d", c)
	}
	var rows int
	must(db.Super.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "Relevés"}.Sanitize()).Scan(&rows))
	if rows != 1 {
		t.Fatalf("l'aperçu a écrit : %d lignes", rows)
	}

	// Logo : envoyé, réduit, servi.
	img := image.NewNRGBA(image.Rect(0, 0, 1200, 300))
	img.Set(10, 10, color.NRGBA{255, 0, 0, 255})
	var pngBuf bytes.Buffer
	must(png.Encode(&pngBuf, img))
	req, _ := http.NewRequest("POST", srv.URL+"/api/forms/"+created["id"]+"/images?kind=logo", &pngBuf)
	resp, err := client.Do(req)
	must(err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`"id":"([0-9a-f-]+)"`).FindStringSubmatch(string(b))
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("envoi du logo : %d %s", resp.StatusCode, b)
	}
	c, body, hdr := do("GET", "/f/"+code+"/img/"+m[1], nil)
	if c != http.StatusOK || hdr.Get("Content-Type") != "image/png" {
		t.Fatalf("logo servi : %d %s", c, hdr.Get("Content-Type"))
	}
	if cfg, _, err := image.DecodeConfig(strings.NewReader(body)); err != nil || cfg.Width > 512 {
		t.Fatalf("logo réduit : %+v %v", cfg, err)
	}
}
