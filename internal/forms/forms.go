// Package forms sert les formulaires (/f/<code>) : la page, d'abord pour
// téléphone, sans le moteur de calcul et sans JavaScript obligatoire ;
// l'envoi, contrôlé ici puis écrit dans le tableau sous l'identité
// technique du formulaire, dans une seule transaction (lattis.form_begin,
// insertion, lattis.form_record) ; le logo et la bannière.
package forms

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Be-Lattis/lattis-server/internal/pipeline"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/userdb"
)

//go:embed web
var webFS embed.FS

var page = template.Must(template.New("").Funcs(template.FuncMap{
	"md": func(s string) template.HTML { return markdown(s, true) },
}).ParseFS(webFS, "web/form.html"))

var codeRe = regexp.MustCompile(`^[a-z2-9]{10}$`)

// Délai minimal entre l'affichage et l'envoi, et durée de vie d'une page.
const (
	minDelay = 3 * time.Second
	maxAge   = 24 * time.Hour
	maxBody  = 64 << 10
	perIP    = 10 // envois par minute et par adresse
)

// Handler sert les formulaires.
type Handler struct {
	Store *store.Store
	Users userdb.Runner
	Keys  *tokens.KeySet
	// Session : utilisateur connecté au site (session de l'administration).
	Session func(*http.Request) (store.User, error)
	// Bearer : utilisateur d'un jeton d'accès de l'application (images).
	Bearer func(*http.Request) (store.User, error)
	// TrustProxy : adresse du client lue dans X-Forwarded-For.
	ClientIP func(*http.Request) string
	Now      func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Routes enregistre les points des formulaires.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /f/{code}", h.show)
	mux.HandleFunc("POST /f/{code}", h.submit)
	mux.HandleFunc("GET /f/{code}/merci", h.thanks)
	mux.HandleFunc("GET /f/{code}/img/{id}", h.image)
	mux.HandleFunc("GET /form-static/{file}", h.static)
	mux.HandleFunc("POST /api/forms/{id}/images", h.upload)
}

func (h *Handler) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	types := map[string]string{"form.css": "text/css; charset=utf-8", "form.js": "text/javascript; charset=utf-8"}
	ct, ok := types[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, _ := webFS.ReadFile("web/" + name)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}

// --- Chargement ----------------------------------------------------------------------

var errNotFound = errors.New("formulaire inconnu")

func (h *Handler) load(ctx context.Context, code, preview string) (*Page, error) {
	if !codeRe.MatchString(code) {
		return nil, errNotFound
	}
	var p Page
	var prev any
	if preview != "" {
		prev = preview
	}
	err := h.Store.AsActor(ctx, "lattisd (formulaires)", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.form_page($1, $2)`, code, prev).Scan(&p)
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "P0002" {
		return nil, errNotFound
	}
	return &p, err
}

// gate : "" si la personne peut répondre, sinon la raison (lattis.form_gate).
func (h *Handler) gate(ctx context.Context, p *Page, user *store.User) (string, error) {
	var uid any
	if user != nil {
		uid = user.ID
	}
	var why *string
	err := h.Store.AsActor(ctx, "lattisd (formulaires)", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.form_gate($1::uuid, $2::uuid)`, p.ID, uid).Scan(&why)
	})
	if why == nil {
		return "", err
	}
	return *why, err
}

func (h *Handler) user(r *http.Request) *store.User {
	if h.Session == nil {
		return nil
	}
	if u, err := h.Session(r); err == nil {
		return &u
	}
	return nil
}

// --- Jeton de page (anti-CSRF et délai minimal) ------------------------------------------

func (h *Handler) key() []byte {
	s := sha256.Sum256(append([]byte("lattis-formulaires\x00"), h.Keys.Signer().Private.Seed()...))
	return s[:]
}

func (h *Handler) sign(s string) string {
	m := hmac.New(sha256.New, h.key())
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (h *Handler) pageToken(code, uid string) string {
	payload := code + "|" + strconv.FormatInt(h.now().UnixMilli(), 10) + "|" + uid
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + h.sign(payload)
}

// checkToken : "" si le jeton va, sinon la clé du message.
func (h *Handler) checkToken(tok, code, uid string) string {
	enc, sig, ok := strings.Cut(tok, ".")
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if !ok || err != nil || !hmac.Equal([]byte(sig), []byte(h.sign(string(b)))) {
		return "expired"
	}
	parts := strings.Split(string(b), "|")
	if len(parts) != 3 || parts[0] != code || parts[2] != uid {
		return "expired"
	}
	ms, _ := strconv.ParseInt(parts[1], 10, 64)
	age := h.now().Sub(time.UnixMilli(ms))
	switch {
	case age < minDelay:
		return "too_fast"
	case age > maxAge:
		return "expired"
	}
	return ""
}

// clientHash : adresse tronquée (/24, /48) et hachée, pour les limites.
func (h *Handler) clientHash(r *http.Request) string {
	ip := net.ParseIP(h.ip(r))
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4.Mask(net.CIDRMask(24, 32))
	} else {
		ip = ip.Mask(net.CIDRMask(48, 128))
	}
	return h.sign("ip:" + ip.String())[:32]
}

func (h *Handler) ip(r *http.Request) string {
	if h.ClientIP != nil {
		return h.ClientIP(r)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (h *Handler) allow(r *http.Request) bool {
	k := h.ip(r)
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hits == nil {
		h.hits = map[string][]time.Time{}
	}
	keep := h.hits[k][:0]
	for _, t := range h.hits[k] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= perIP {
		h.hits[k] = keep
		return false
	}
	h.hits[k] = append(keep, now)
	if len(h.hits) > 10000 { // borne la mémoire : on repart de zéro
		h.hits = map[string][]time.Time{k: h.hits[k]}
	}
	return true
}

// --- Affichage -----------------------------------------------------------------------------

type viewField struct {
	Section                       bool
	Title, Description            string
	Name, ID, Label, Help         string
	Placeholder, Widget           string
	Required, Locked              bool
	Value, Lat, Lon               string
	Options                       []string
	Error                         string
	Min, Max, Step                string
	MaxLength                     int
	InputType, InputMode, AutoKey string
	Unit                          string
}

type view struct {
	Lang, Title, Accent, Logo, Banner, Submit, Token, Action string
	Intro, Privacy, Thanks                                   string
	T                                                        tr
	Fields                                                   []viewField
	Errors                                                   bool
	Message, Kind                                            string
	Preview, PreviewDone                                     bool
	Another                                                  string
	Server                                                   string
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := page.ExecuteTemplate(w, name, v); err != nil {
		slog.Error("page de formulaire", "err", err)
	}
}

func (h *Handler) message(w http.ResponseWriter, status int, p *Page, key string) {
	t := texts["fr"]
	v := view{Lang: "fr", T: t, Title: t["not_found_title"], Message: t[key], Kind: key}
	if p != nil {
		t = lang(p.Definition.Lang)
		v.Lang, v.T = p.Definition.Lang, t
		v.Title, v.Accent, v.Server = p.Definition.Appearance.Title, p.Definition.Appearance.Accent, p.ServerName
		v.Message = t[key]
		if key == "closed" && p.Definition.Appearance.ClosedMessage != "" {
			v.Intro = p.Definition.Appearance.ClosedMessage
			v.Message = ""
		}
		v.Logo, v.Banner = h.imageURL(p, "logo"), h.imageURL(p, "banner")
	}
	if v.Lang == "" {
		v.Lang = "fr"
	}
	h.render(w, status, "message", v)
}

func (h *Handler) imageURL(p *Page, kind string) string {
	if id := p.Images[kind]; id != "" {
		return "/f/" + p.Code + "/img/" + id
	}
	return ""
}

func (h *Handler) form(p *Page, r *http.Request, user *store.User, values url.Values, errs map[string]string) view {
	t := lang(p.Definition.Lang)
	a := p.Definition.Appearance
	uid := ""
	respondent := ""
	if user != nil {
		uid, respondent = user.ID, user.Email
	}
	v := view{Lang: p.Definition.Lang, T: t, Title: a.Title, Accent: a.Accent, Intro: a.Intro, Privacy: a.Privacy,
		Submit: a.SubmitLabel, Token: h.pageToken(p.Code, uid), Action: r.URL.RequestURI(), Preview: p.Preview,
		Server: p.ServerName, Errors: len(errs) > 0, Logo: h.imageURL(p, "logo"), Banner: h.imageURL(p, "banner")}
	if v.Lang == "" {
		v.Lang = "fr"
	}
	if v.Submit == "" {
		v.Submit = t["submit"]
	}
	q := r.URL.Query()
	for _, f := range p.Definition.Fields {
		if f.Kind == "section" {
			v.Fields = append(v.Fields, viewField{Section: true, Title: f.Title, Description: f.Description})
			continue
		}
		colID := f.Column
		lat, _, isPos := f.position()
		if isPos {
			colID = lat
		}
		c, ok := p.Columns[colID]
		if !ok || f.Hidden {
			continue
		}
		list := p.Lists[colID]
		vf := viewField{Name: "c_" + colID, ID: "f_" + colID, Label: f.Label, Help: f.Help, Placeholder: f.Placeholder,
			Required: f.Required, Widget: f.widget(c, list), MaxLength: f.MaxLength, Error: errs[colID], Unit: f.Unit}
		if isPos {
			vf.Widget = "position"
		}
		if vf.Label == "" {
			vf.Label = c.Name
		}
		for _, o := range list {
			vf.Options = append(vf.Options, fmt.Sprint(o))
		}
		if f.Min != nil {
			vf.Min = fmtNum(*f.Min)
		}
		if f.Max != nil {
			vf.Max = fmtNum(*f.Max)
		}
		if f.Step != nil {
			vf.Step = fmtNum(*f.Step)
		} else if c.Type == "decimal" {
			vf.Step = "any"
		}
		switch {
		case vf.Widget == "number":
			vf.InputType, vf.InputMode = "text", "decimal"
			if c.Type == "integer" {
				vf.InputMode = "numeric"
			}
		case vf.Widget == "date":
			vf.InputType = "date"
		case vf.Widget == "datetime":
			vf.InputType = "datetime-local"
		case f.Format == "email":
			vf.InputType, vf.AutoKey = "email", "email"
		case f.Format == "phone":
			vf.InputType, vf.AutoKey = "tel", "tel"
		case f.Format == "url":
			vf.InputType = "url"
		default:
			vf.InputType = "text"
		}
		// Valeur : saisie renvoyée, sinon paramètre du lien, sinon défaut.
		pre, fromLink := "", false
		if f.Param != "" && q.Has(f.Param) {
			pre, fromLink = q.Get(f.Param), true
		}
		switch {
		case values != nil:
			vf.Value = values.Get(vf.Name)
			vf.Lat, vf.Lon = values.Get(vf.Name+"_lat"), values.Get(vf.Name+"_lon")
		case fromLink:
			vf.Value = pre
		default:
			vf.Value = defaultValue(f.Default, respondent, h.now())
		}
		if fromLink && f.Locked {
			vf.Locked, vf.Value = true, pre
		}
		v.Fields = append(v.Fields, vf)
	}
	return v
}

func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
	p, err := h.load(r.Context(), r.PathValue("code"), r.URL.Query().Get("p"))
	if err != nil {
		h.fail(w, err)
		return
	}
	user := h.user(r)
	if !p.Preview && !h.allowed(w, r, p, user) {
		return
	}
	h.render(w, http.StatusOK, "form", h.form(p, r, user, nil, nil))
}

// allowed répond à la place de la page quand la personne ne peut pas
// répondre (connexion, introuvable, fermé…).
func (h *Handler) allowed(w http.ResponseWriter, r *http.Request, p *Page, user *store.User) bool {
	why, err := h.gate(r.Context(), p, user)
	if err != nil {
		h.fail(w, err)
		return false
	}
	switch why {
	case "":
		return true
	case "login":
		http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
	case "not_open", "closed", "full":
		h.message(w, http.StatusGone, p, why)
	case "already":
		h.message(w, http.StatusConflict, p, "already")
	default: // forbidden, public_disabled, unknown : même page qu'un formulaire inconnu
		h.message(w, http.StatusNotFound, nil, "not_found")
	}
	return false
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotFound) {
		h.message(w, http.StatusNotFound, nil, "not_found")
		return
	}
	slog.Error("formulaire", "err", err)
	h.message(w, http.StatusServiceUnavailable, nil, "error")
}

// --- Envoi --------------------------------------------------------------------------------

func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "envoi trop gros ou illisible", http.StatusRequestEntityTooLarge)
		return
	}
	code := r.PathValue("code")
	p, err := h.load(r.Context(), code, r.URL.Query().Get("p"))
	if err != nil {
		h.fail(w, err)
		return
	}
	user := h.user(r)
	t := lang(p.Definition.Lang)
	// Pot de miel : un humain ne remplit pas ce champ caché.
	if r.PostForm.Get("website") != "" {
		http.Redirect(w, r, "/f/"+code+"/merci", http.StatusSeeOther)
		return
	}
	if !h.allow(r) {
		w.Header().Set("Retry-After", "60")
		h.message(w, http.StatusTooManyRequests, p, "rate")
		return
	}
	if !p.Preview && !h.allowed(w, r, p, user) {
		return
	}
	uid := ""
	if user != nil {
		uid = user.ID
	}
	values, errs := h.collect(p, r, user, t)
	if why := h.checkToken(r.PostForm.Get("t"), code, uid); why != "" {
		v := h.form(p, r, user, r.PostForm, errs)
		v.Errors, v.Message = true, t[why]
		h.render(w, http.StatusBadRequest, "form", v)
		return
	}
	if len(errs) > 0 {
		v := h.form(p, r, user, r.PostForm, errs)
		v.Message = t["errors"]
		h.render(w, http.StatusBadRequest, "form", v)
		return
	}
	if p.Preview {
		v := view{Lang: p.Definition.Lang, T: t, Title: p.Definition.Appearance.Title, Accent: p.Definition.Appearance.Accent,
			Thanks: p.Definition.Appearance.Thanks, PreviewDone: true, Server: p.ServerName}
		h.render(w, http.StatusOK, "thanks", v)
		return
	}
	if err := h.write(r.Context(), p, values, user, h.clientHash(r)); err != nil {
		var pe *pgconn.PgError
		key := ""
		status := http.StatusBadRequest
		switch {
		case errors.As(err, &pe) && pe.Code == "23505":
			key, status = "taken", http.StatusConflict
		case errors.As(err, &pe) && pe.Code == "P0001":
			h.message(w, http.StatusConflict, p, "already")
			return
		case errors.As(err, &pe) && pe.Code == "55000":
			h.message(w, http.StatusGone, p, "closed")
			return
		default:
			slog.Error("envoi de formulaire", "form", p.ID, "err", err)
			key, status = "error", http.StatusServiceUnavailable
		}
		v := h.form(p, r, user, r.PostForm, nil)
		v.Errors, v.Message = true, t[key]
		h.render(w, status, "form", v)
		return
	}
	dest := "/f/" + code + "/merci"
	if p.Definition.Appearance.Another {
		dest += "?autre=1"
		if qs := r.URL.Query(); len(qs) > 0 {
			dest += "&" + url.Values{"lien": {qs.Encode()}}.Encode()
		}
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// collect contrôle les champs envoyés ; values : colonne → valeur pour
// PostgreSQL ("" : vide).
func (h *Handler) collect(p *Page, r *http.Request, user *store.User, t tr) (map[string]string, map[string]string) {
	values, errs := map[string]string{}, map[string]string{}
	q := r.URL.Query()
	respondent := ""
	if user != nil {
		respondent = user.Email
	}
	for _, f := range p.Definition.Fields {
		if f.Kind == "section" {
			continue
		}
		if latCol, lonCol, isPos := f.position(); isPos {
			name := "c_" + latCol
			lat, lon, msg := checkPosition(r.PostForm.Get(name+"_lat"), r.PostForm.Get(name+"_lon"), t)
			switch {
			case msg != "":
				errs[latCol] = msg
			case lat == "" && f.Required:
				errs[latCol] = t["required"]
			case lat != "" && lonCol != "":
				values[latCol], values[lonCol] = lat, lon
			case lat != "":
				values[latCol] = lat + ", " + lon
			}
			continue
		}
		c, ok := p.Columns[f.Column]
		if !ok {
			continue
		}
		name := "c_" + f.Column
		raw := r.PostForm.Get(name)
		switch {
		case f.Locked && f.Param != "" && q.Has(f.Param):
			raw = q.Get(f.Param) // fixé par le lien : revérifié, pas repris du corps
		case f.Hidden:
			raw = defaultValue(f.Default, respondent, h.now())
		}
		val, msg := checkValue(f, c, p.Lists[f.Column], raw, t)
		switch {
		case msg != "":
			errs[f.Column] = msg
		case f.Required && c.Type == "boolean" && f.widget(c, nil) == "checkbox" && val != "true":
			errs[f.Column] = t["must_check"]
		case f.Required && val == "":
			errs[f.Column] = t["required"]
		default:
			if val == "" && c.Type == "boolean" && f.widget(c, nil) == "checkbox" {
				val = "false"
			}
			values[f.Column] = val
		}
	}
	return values, errs
}

var casts = map[string]string{"text": "text", "integer": "bigint", "decimal": "numeric", "date": "date",
	"datetime": "timestamptz", "boolean": "boolean"}

// write insère la ligne sous l'identité du formulaire, dans une seule
// transaction avec les contrôles qui font foi (lattis.form_begin) et le
// journal des réponses (lattis.form_record).
func (h *Handler) write(ctx context.Context, p *Page, values map[string]string, user *store.User, clientHash string) error {
	identity, err := h.Store.User(ctx, p.Identity)
	if err != nil {
		return err
	}
	var respondent any
	email := ""
	if user != nil {
		respondent, email = user.ID, user.Email
	}
	return h.Users.As(ctx, identity, func(tx pgx.Tx) error {
		var b struct {
			Schema  string             `json:"schema"`
			Table   string             `json:"table"`
			MaxPos  *string            `json:"max_pos"`
			Numbers map[string]float64 `json:"numbers"`
			Columns map[string]Column  `json:"columns"`
		}
		if err := tx.QueryRow(ctx, `SELECT lattis.form_begin($1::uuid)`, p.ID).Scan(&b); err != nil {
			return err
		}
		for _, sv := range p.Definition.ServerValues {
			c, ok := b.Columns[sv.Column]
			if !ok {
				continue
			}
			switch sv.Value.Token {
			case "received_at":
				if c.Type == "date" {
					values[sv.Column] = h.now().Format("2006-01-02")
				} else {
					// Heure locale du serveur, sans fuseau, marquée UTC
					// comme les dates-heures de Lattis.
					values[sv.Column] = h.now().Format("2006-01-02T15:04:05") + "Z"
				}
			case "respondent":
				values[sv.Column] = email
			case "form_title":
				values[sv.Column] = p.Definition.Appearance.Title
			case "next_number":
				values[sv.Column] = strconv.FormatFloat(b.Numbers[sv.Column], 'f', -1, 64)
			default:
				if sv.Value.Fixed != nil {
					values[sv.Column] = fmt.Sprint(sv.Value.Fixed)
				}
			}
		}
		last := ""
		if b.MaxPos != nil {
			last = *b.MaxPos
		}
		cols := []string{"lattis_id", "lattis_pos"}
		marks := []string{"uuidv7()", "$1"}
		args := []any{pipeline.NextPos(last)}
		for id, v := range values {
			c, ok := b.Columns[id]
			if !ok {
				continue
			}
			cols = append(cols, pgx.Identifier{c.Name}.Sanitize())
			args = append(args, nullable(v))
			marks = append(marks, fmt.Sprintf("$%d::%s", len(args), casts[c.Type]))
		}
		var row string
		if err := tx.QueryRow(ctx, `INSERT INTO `+pgx.Identifier{b.Schema, b.Table}.Sanitize()+` (`+strings.Join(cols, ", ")+
			`) VALUES (`+strings.Join(marks, ", ")+`) RETURNING lattis_id::text`, args...).Scan(&row); err != nil {
			return err
		}
		var hash any
		if clientHash != "" {
			hash = clientHash
		}
		_, err := tx.Exec(ctx, `SELECT lattis.form_record($1::uuid, $2::uuid, $3::uuid, $4, NULL)`, p.ID, row, respondent, hash)
		return err
	})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (h *Handler) thanks(w http.ResponseWriter, r *http.Request) {
	p, err := h.load(r.Context(), r.PathValue("code"), "")
	if err != nil {
		h.fail(w, err)
		return
	}
	t := lang(p.Definition.Lang)
	v := view{Lang: p.Definition.Lang, T: t, Title: p.Definition.Appearance.Title, Accent: p.Definition.Appearance.Accent,
		Thanks: p.Definition.Appearance.Thanks, Server: p.ServerName, Logo: h.imageURL(p, "logo")}
	if r.URL.Query().Get("autre") == "1" && p.Definition.Appearance.Another {
		v.Another = "/f/" + p.Code
		if l := r.URL.Query().Get("lien"); l != "" {
			if _, err := url.ParseQuery(l); err == nil {
				v.Another += "?" + l
			}
		}
	}
	if v.Lang == "" {
		v.Lang = "fr"
	}
	h.render(w, http.StatusOK, "thanks", v)
}

// --- Images -----------------------------------------------------------------------------

func (h *Handler) image(w http.ResponseWriter, r *http.Request) {
	var ct string
	var data []byte
	err := h.Store.DB.QueryRow(r.Context(), `SELECT i.content_type, i.data FROM lattis.form_images i
		JOIN lattis.forms f ON f.id = i.form_id WHERE f.code = $1 AND i.id::text = $2`, r.PathValue("code"), r.PathValue("id")).Scan(&ct, &data)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// upload : logo ou bannière envoyé par l'application (jeton d'accès),
// réduit et réencodé ici (métadonnées retirées), enregistré sous l'identité
// de l'utilisateur (propriétaire ou concepteur du classeur).
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	jsonErr := func(status int, code, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": msg})
	}
	if h.Bearer == nil {
		jsonErr(http.StatusServiceUnavailable, "unavailable", "images indisponibles")
		return
	}
	u, err := h.Bearer(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		jsonErr(http.StatusUnauthorized, "invalid_token", "jeton refusé")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "logo" && kind != "banner" {
		jsonErr(http.StatusBadRequest, "invalid_request", "kind : logo ou banner")
		return
	}
	data, ct, err := shrink(http.MaxBytesReader(w, r.Body, maxImage), kind)
	switch {
	case errors.Is(err, errTooBig):
		jsonErr(http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("image trop grosse (plus de %d Mo)", maxImage>>20))
		return
	case err != nil:
		jsonErr(http.StatusUnsupportedMediaType, "unsupported", "image PNG ou JPEG attendue")
		return
	}
	var id string
	err = h.Users.As(r.Context(), u, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT lattis.form_set_image($1::uuid, $2, $3, $4)::text`, r.PathValue("id"), kind, ct, data).Scan(&id)
	})
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe) && pe.Code == "42501":
		jsonErr(http.StatusForbidden, "forbidden", pe.Message)
		return
	case errors.As(err, &pe) && (pe.Code == "P0002" || pe.Code == "22P02"):
		jsonErr(http.StatusNotFound, "not_found", "formulaire inconnu")
		return
	case err != nil:
		slog.Error("image de formulaire", "err", err)
		jsonErr(http.StatusServiceUnavailable, "unavailable", "enregistrement impossible")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "content_type": ct, "bytes": len(data)})
}
