package forms

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"net/http"

	"github.com/Be-Lattis/lattis-server/internal/store"
)

// Formulaire installable comme application : manifeste propre à chaque
// formulaire (nom, icône, adresse de départ), icône aussi utilisée en favicon.
// Sans service worker : le formulaire reste en ligne, et aucun cache ne peut
// garder une ancienne version.

// iconServer : icône d'un formulaire sans logo (logo du serveur).
const iconServer = "serveur"

var iconSizes = map[string]int{"32": 32, "192": 192, "512": 512}

// accentColors : couleur de la barre de l'application (form.css).
var accentColors = map[string]string{"green": "#217346", "teal": "#0f766e", "blue": "#1d5fbf", "indigo": "#4338ca",
	"purple": "#7e22ce", "red": "#b42318", "orange": "#b45309", "gray": "#475467", "slate": "#475467"}

func accentColor(accent string) string {
	if c, ok := accentColors[accent]; ok {
		return c
	}
	return accentColors["green"]
}

func iconURL(p *Page, size string) string {
	id := p.Images["logo"]
	if id == "" {
		id = iconServer
	}
	return "/f/" + p.Code + "/icon/" + id + "/" + size
}

// appHead : favicon et manifeste de la page d'un formulaire (pas d'un aperçu).
func appHead(p *Page, v *view) {
	v.Favicon = iconURL(p, "192")
	v.Theme = accentColor(p.Definition.Appearance.Accent)
	if !p.Preview {
		v.Manifest = "/f/" + p.Code + "/manifest.webmanifest"
	}
}

// manifest : GET /f/{code}/manifest.webmanifest. Même réserve que la page :
// un formulaire réservé ne dit pas son nom à qui ne peut pas l'ouvrir.
func (h *Handler) manifest(w http.ResponseWriter, r *http.Request) {
	p, err := h.load(r.Context(), r.PathValue("code"), "")
	if err != nil {
		h.fail(w, err)
		return
	}
	why, err := h.gate(r.Context(), p, h.user(r))
	if err != nil {
		h.fail(w, err)
		return
	}
	switch why {
	case "", "not_open", "closed", "full", "already":
	default:
		http.NotFound(w, r)
		return
	}
	a := p.Definition.Appearance
	name := a.Title
	if name == "" {
		name = lang(p.Definition.Lang)["powered"]
	}
	start := "/f/" + p.Code
	icons := []map[string]string{}
	for _, s := range []string{"192", "512"} {
		icons = append(icons, map[string]string{"src": iconURL(p, s), "sizes": s + "x" + s, "type": "image/png", "purpose": "any"})
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": start, "name": name, "short_name": name, "start_url": start, "scope": start,
		"display": "standalone", "lang": p.Definition.Lang, "theme_color": accentColor(a.Accent), "background_color": "#ffffff", "icons": icons})
}

// icon : GET /f/{code}/icon/{id}/{size} : logo du formulaire (ou du serveur)
// centré sur un carré transparent, comme l'exigent les applications
// installées. L'identifiant change avec l'image : mise en cache durable.
func (h *Handler) icon(w http.ResponseWriter, r *http.Request) {
	size, ok := iconSizes[r.PathValue("size")]
	id := r.PathValue("id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	var data []byte
	var err error
	if id == iconServer {
		cache := "public, max-age=300"
		if _, data, err = h.Store.ServerLogo(r.Context()); errors.Is(err, store.ErrNotFound) {
			data, err = h.DefaultIcon, nil
		}
		w.Header().Set("Cache-Control", cache)
	} else {
		err = h.Store.DB.QueryRow(r.Context(), `SELECT i.data FROM lattis.form_images i
			JOIN lattis.forms f ON f.id = i.form_id WHERE f.code = $1 AND i.id::text = $2`, r.PathValue("code"), id).Scan(&data)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out, err := squareIcon(data, size)
	if err != nil {
		// Logo du serveur dans un format non décodé : icône Lattis.
		if out, err = squareIcon(h.DefaultIcon, size); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(out)
}

// squareIcon réduit ou agrandit l'image au côté size, centrée sur un carré
// transparent, en PNG.
func squareIcon(data []byte, size int) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	if b.Dx() > size || b.Dy() > size {
		src = fit(src, size, size)
		b = src.Bounds()
	} else if s := max(b.Dx(), b.Dy()); s > 0 && s < size {
		src = enlarge(src, size*b.Dx()/s, size*b.Dy()/s)
		b = src.Bounds()
	}
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	at := image.Rect((size-b.Dx())/2, (size-b.Dy())/2, (size+b.Dx())/2, (size+b.Dy())/2)
	draw.Draw(dst, at, src, b.Min, draw.Over)
	var out bytes.Buffer
	err = png.Encode(&out, dst)
	return out.Bytes(), err
}

// enlarge agrandit l'image (plus proche voisin).
func enlarge(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
	for y := 0; y < dst.Rect.Dy(); y++ {
		for x := 0; x < dst.Rect.Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/dst.Rect.Dx(), b.Min.Y+y*b.Dy()/dst.Rect.Dy()))
		}
	}
	return dst
}
