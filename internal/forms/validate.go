package forms

import (
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Page : ce que lattis.form_page renvoie.
type Page struct {
	ID               string            `json:"id"`
	Code             string            `json:"code"`
	State            string            `json:"state"`
	PauseReason      *string           `json:"pause_reason"`
	Access           string            `json:"access"`
	Definition       Definition        `json:"definition"`
	Columns          map[string]Column `json:"columns"`
	Lists            map[string][]any  `json:"lists"`
	MaxPerRespondent *int              `json:"max_per_respondent"`
	Responses        int               `json:"responses"`
	Identity         string            `json:"identity"`
	Workbook         string            `json:"workbook"`
	Preview          bool              `json:"preview"`
	Images           map[string]string `json:"images"`
	ServerName       string            `json:"server_name"`
}

// Column : colonne du tableau (lattis_meta_columns).
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"` // text, integer, decimal, date, datetime, boolean
}

// Definition : définition JSON d'un formulaire (format 1).
type Definition struct {
	Lang         string        `json:"lang"`
	Appearance   Appearance    `json:"appearance"`
	Fields       []Field       `json:"fields"`
	ServerValues []ServerValue `json:"server_values"`
}

type Appearance struct {
	Title         string `json:"title"`
	Intro         string `json:"intro"`
	Logo          string `json:"logo"`
	Banner        string `json:"banner"`
	Accent        string `json:"accent"`
	SubmitLabel   string `json:"submit_label"`
	Thanks        string `json:"thanks"`
	Another       bool   `json:"another"`
	ClosedMessage string `json:"closed_message"`
	Privacy       string `json:"privacy"`
}

type Field struct {
	Kind        string `json:"kind"` // section, ou vide pour un champ
	Title       string `json:"title"`
	Description string `json:"description"`
	Column      string `json:"column"`
	LonColumn   string `json:"lon_column"`
	// Position (kind « position ») : deux colonnes décimales, ou Column texte.
	Latitude    string   `json:"latitude"`
	Longitude   string   `json:"longitude"`
	Unit        string   `json:"unit"`
	Label       string   `json:"label"`
	Help        string   `json:"help"`
	Placeholder string   `json:"placeholder"`
	Required    bool     `json:"required"`
	Widget      string   `json:"widget"`
	Default     *Default `json:"default"`
	Min         *float64 `json:"min"`
	Max         *float64 `json:"max"`
	Step        *float64 `json:"step"`
	MaxLength   int      `json:"max_length"`
	Format      string   `json:"format"`
	Param       string   `json:"param"`
	Locked      bool     `json:"locked"`
	Hidden      bool     `json:"hidden"`
}

type Default struct {
	Token string `json:"token"`
	Fixed any    `json:"fixed"`
}

type ServerValue struct {
	Column string  `json:"column"`
	Value  Default `json:"value"`
}

// position : champ « Ma position » ; lat et lon : colonnes à remplir (lon
// vide : une colonne texte « lat, lon »).
func (f Field) position() (lat, lon string, ok bool) {
	switch {
	case f.Kind == "position" && f.Latitude != "":
		return f.Latitude, f.Longitude, true
	case f.Kind == "position" || f.Widget == "position":
		return f.Column, f.LonColumn, true
	}
	return "", "", false
}

// widget : type de champ effectif.
func (f Field) widget(c Column, list []any) string {
	if f.Widget != "" && f.Widget != "list" {
		return f.Widget
	}
	if list != nil {
		if len(list) <= 7 {
			return "radio"
		}
		return "select"
	}
	switch c.Type {
	case "integer", "decimal":
		return "number"
	case "date":
		return "date"
	case "datetime":
		return "datetime"
	case "boolean":
		return "checkbox"
	}
	return "text"
}

var phoneRe = regexp.MustCompile(`^[+0-9 ().-]{4,30}$`)

// clean retire les caractères de contrôle (sauf saut de ligne et tabulation).
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

// checkValue contrôle une valeur saisie et la rend sous la forme que
// PostgreSQL attend pour la colonne ("" : vide). msg : raison du refus.
func checkValue(f Field, c Column, list []any, raw string, t tr) (val string, msg string) {
	raw = strings.TrimSpace(clean(raw))
	if list != nil && raw != "" {
		for _, v := range list {
			if s := fmt.Sprint(v); strings.EqualFold(s, raw) {
				return s, ""
			}
		}
		return "", t["not_in_list"]
	}
	switch c.Type {
	case "boolean":
		switch strings.ToLower(raw) {
		case "on", "true", "1", "oui", "yes":
			return "true", ""
		case "false", "0", "non", "no":
			return "false", ""
		case "":
			return "", ""
		}
		return "", t["bad_value"]
	case "integer", "decimal":
		if raw == "" {
			return "", ""
		}
		s := strings.NewReplacer(" ", "", " ", "", " ", "").Replace(raw)
		s = strings.Replace(s, ",", ".", 1)
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return "", t["number"]
		}
		if c.Type == "integer" && n != math.Trunc(n) {
			return "", t["integer"]
		}
		if f.Min != nil && n < *f.Min {
			return "", fmt.Sprintf(t["min"], fmtNum(*f.Min))
		}
		if f.Max != nil && n > *f.Max {
			return "", fmt.Sprintf(t["max"], fmtNum(*f.Max))
		}
		return s, ""
	case "date":
		if raw == "" {
			return "", ""
		}
		for _, l := range []string{"2006-01-02", "02/01/2006"} {
			if d, err := time.ParseInLocation(l, raw, time.Local); err == nil {
				return d.Format("2006-01-02"), ""
			}
		}
		return "", t["date"]
	case "datetime":
		if raw == "" {
			return "", ""
		}
		for _, l := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "02/01/2006 15:04"} {
			if d, err := time.ParseInLocation(l, raw, time.Local); err == nil {
				return d.Format(time.RFC3339), ""
			}
		}
		return "", t["datetime"]
	}
	// Texte.
	max := f.MaxLength
	if max <= 0 || max > 10000 {
		max = 10000
	}
	if len([]rune(raw)) > max {
		return "", fmt.Sprintf(t["too_long"], max)
	}
	if raw != "" {
		switch f.Format {
		case "email":
			if a, err := mail.ParseAddress(raw); err != nil || a.Address != raw {
				return "", t["email"]
			}
		case "phone":
			if !phoneRe.MatchString(raw) {
				return "", t["phone"]
			}
		case "url":
			if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") {
				return "", t["url"]
			}
		}
	}
	return raw, ""
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// checkPosition : latitude et longitude saisies (bouton « Ma position »).
func checkPosition(lat, lon string, t tr) (string, string, string) {
	lat, lon = strings.TrimSpace(lat), strings.TrimSpace(lon)
	if lat == "" && lon == "" {
		return "", "", ""
	}
	a, err1 := strconv.ParseFloat(strings.Replace(lat, ",", ".", 1), 64)
	b, err2 := strconv.ParseFloat(strings.Replace(lon, ",", ".", 1), 64)
	if err1 != nil || err2 != nil || a < -90 || a > 90 || b < -180 || b > 180 {
		return "", "", t["position"]
	}
	return strconv.FormatFloat(a, 'f', 6, 64), strconv.FormatFloat(b, 'f', 6, 64), ""
}

// defaultValue : valeur pré-remplie d'un champ.
func defaultValue(d *Default, respondent string, now time.Time) string {
	if d == nil {
		return ""
	}
	switch d.Token {
	case "today":
		return now.Format("2006-01-02")
	case "now":
		return now.Format("2006-01-02T15:04")
	case "year":
		return now.Format("2006")
	case "respondent":
		return respondent
	}
	if d.Fixed != nil {
		return fmt.Sprint(d.Fixed)
	}
	return ""
}
